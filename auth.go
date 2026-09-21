// 用户中心令牌校验：本地验签，不回源。
//
// 浏览器直接用账号密码向用户中心换 access_token，随后带着它调用本服务的 /api/*；
// 本服务只需拉一次用户中心的 JWKS 公钥，之后本地验签即可，用户中心下线也不影响已登录用户。
//
// 只依赖标准库：Ed25519 验签用 crypto/ed25519，JWT 结构自己解析（结构很固定，无需引入库）。
//
// 本面板只对管理员开放：令牌除验签、校验 aud 外，还要求 role 声明为 admin。
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// 时钟偏移容忍：签发方与校验方的系统时间可能有几十秒差异
	clockSkew = 30 * time.Second
	// JWKS 缓存时长。公钥轮换不频繁，取 token 里未知 kid 时再强制刷新一次
	jwksTTL = time.Hour
)

// authConfig 鉴权配置。UserCenterURL 为空表示不启用鉴权（保留可一键关闭的退路）。
type authConfig struct {
	UserCenterURL string
	App           string
	AllowedEmails []string
}

// enabled 是否启用鉴权
func (c authConfig) enabled() bool { return strings.TrimSpace(c.UserCenterURL) != "" }

// tokenClaims 访问令牌里我们关心的声明
type tokenClaims struct {
	Iss      string `json:"iss"`
	Sub      string `json:"sub"`
	Aud      string `json:"aud"`
	Iat      int64  `json:"iat"`
	Exp      int64  `json:"exp"`
	Jti      string `json:"jti"`
	DeviceID string `json:"device_id"`
	Scope    string `json:"scope"`
	Email    string `json:"email"`
	Role     string `json:"role"`
}

// jwksDoc 用户中心 /.well-known/jwks.json 的结构
type jwksDoc struct {
	Keys []struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Kid string `json:"kid"`
	} `json:"keys"`
}

type authenticator struct {
	cfg  authConfig
	http *http.Client

	mu        sync.RWMutex
	keys      map[string]ed25519.PublicKey
	only      ed25519.PublicKey // 只有一个公钥时作为无 kid 令牌的兜底
	fetchedAt time.Time
}

func newAuthenticator(cfg authConfig, client *http.Client) *authenticator {
	a := &authenticator{cfg: cfg, http: client, keys: map[string]ed25519.PublicKey{}}
	if a.cfg.App == "" {
		a.cfg.App = "cicd"
	}
	return a
}

// fetchJWKS 拉取并缓存公钥集
func (a *authenticator) fetchJWKS(ctx context.Context, force bool) error {
	a.mu.RLock()
	fresh := !force && len(a.keys) > 0 && time.Since(a.fetchedAt) < jwksTTL
	a.mu.RUnlock()
	if fresh {
		return nil
	}

	url := strings.TrimRight(a.cfg.UserCenterURL, "/") + "/.well-known/jwks.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("拉取 JWKS 失败: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("拉取 JWKS 失败: HTTP %d", res.StatusCode)
	}

	var doc jwksDoc
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		return fmt.Errorf("解析 JWKS 失败: %w", err)
	}

	keys := make(map[string]ed25519.PublicKey, len(doc.Keys))
	var single ed25519.PublicKey
	for _, k := range doc.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.X == "" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			log.Printf("忽略无法解析的 JWKS 公钥 kid=%s: %v", k.Kid, err)
			continue
		}
		pub := ed25519.PublicKey(raw)
		if k.Kid != "" {
			keys[k.Kid] = pub
		}
		single = pub
	}
	if len(keys) == 0 && single == nil {
		return errors.New("JWKS 中没有可用的 Ed25519 公钥")
	}

	a.mu.Lock()
	a.keys, a.only, a.fetchedAt = keys, single, time.Now()
	a.mu.Unlock()
	return nil
}

// lookupKey 按 kid 找公钥；找不到则强制刷新一次（应对公钥轮换）
func (a *authenticator) lookupKey(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	if err := a.fetchJWKS(ctx, false); err != nil {
		return nil, err
	}
	if pub := a.keyFor(kid); pub != nil {
		return pub, nil
	}
	if err := a.fetchJWKS(ctx, true); err != nil {
		return nil, err
	}
	if pub := a.keyFor(kid); pub != nil {
		return pub, nil
	}
	return nil, fmt.Errorf("未知的签名密钥 kid=%q", kid)
}

func (a *authenticator) keyFor(kid string) ed25519.PublicKey {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if kid != "" {
		if pub, ok := a.keys[kid]; ok {
			return pub
		}
		return nil
	}
	return a.only
}

// verify 校验令牌，成功返回声明
func (a *authenticator) verify(ctx context.Context, token string) (*tokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("令牌格式不正确")
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("令牌头部无法解码")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, errors.New("令牌头部无法解析")
	}
	if header.Alg != "EdDSA" {
		return nil, fmt.Errorf("不支持的签名算法 %q", header.Alg)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("令牌签名无法解码")
	}
	pub, err := a.lookupKey(ctx, header.Kid)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("令牌签名校验失败")
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("令牌载荷无法解码")
	}
	var claims tokenClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, errors.New("令牌载荷无法解析")
	}

	now := time.Now()
	if claims.Exp == 0 || now.After(time.Unix(claims.Exp, 0).Add(clockSkew)) {
		return nil, errors.New("令牌已过期")
	}
	if claims.Aud != a.cfg.App {
		return nil, fmt.Errorf("令牌不适用于本服务（aud=%q）", claims.Aud)
	}

	// 本面板只对管理员开放。用户中心每次签发令牌都会带上实时 role，
	// 因此这里只看声明即可；角色被降级后旧令牌仍会带 admin，
	// 但访问令牌 15 分钟就过期，重启登录即收敛。
	if claims.Role != "admin" {
		return nil, fmt.Errorf("%w：%s", errNotAdmin, claims.Email)
	}

	// 邮箱白名单：在「必须是管理员」之上再收窄到具体账号；未配置则放行所有管理员
	if len(a.cfg.AllowedEmails) > 0 {
		ok := false
		for _, e := range a.cfg.AllowedEmails {
			if strings.EqualFold(strings.TrimSpace(e), claims.Email) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w：%s", errNotAllowed, claims.Email)
		}
	}
	return &claims, nil
}

// errNotAllowed 令牌有效但账号不在白名单内。与「令牌无效」区分开：
// 前者重试/刷新都没有意义，应直接告知拒绝，而不是让前端白刷一次令牌。
var errNotAllowed = errors.New("账号不在允许名单内")

// errNotAdmin 令牌有效但账号不是管理员。同样不该用 401 触发前端刷新。
var errNotAdmin = errors.New("需要管理员权限")

// bearerToken 从 Authorization 头取出令牌
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	fields := strings.Fields(h)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return fields[1]
}

// verifyRequest 校验请求携带的令牌
func (a *authenticator) verifyRequest(r *http.Request) (*tokenClaims, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, errors.New("缺少访问令牌")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	return a.verify(ctx, token)
}

// authenticate 校验请求携带的令牌，失败返回 nil（/api/health 据此判断是否下发给本管理员看配置明细）
//
// 刻意不打日志：容器的 healthcheck 每 30 秒无令牌探一次 /api/health，
// 若在此记录会持续刷屏、淹没真实告警。真正的拒绝日志在 wrap 里。
func (a *authenticator) authenticate(r *http.Request) *tokenClaims {
	if !a.cfg.enabled() {
		return nil
	}
	claims, err := a.verifyRequest(r)
	if err != nil {
		return nil
	}
	return claims
}

// wrap 保护需要管理员身份的接口。未启用鉴权时原样放行。
func (a *authenticator) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.cfg.enabled() {
			next(w, r)
			return
		}
		_, err := a.verifyRequest(r)
		if err == nil {
			next(w, r)
			return
		}
		log.Printf("拒绝访问 %s: %v", r.URL.Path, err)

		if errors.Is(err, errNotAllowed) || errors.Is(err, errNotAdmin) {
			// 令牌本身有效，只是账号无权限：重试无意义，不用 401 触发前端刷新
			writeErr(w, http.StatusForbidden, err)
			return
		}
		// 浏览器据此区分「需要登录」与其它错误，进而弹出登录框
		w.Header().Set("WWW-Authenticate", `Bearer realm="user-center"`)
		writeErr(w, http.StatusUnauthorized, errors.New("需要登录"))
	}
}
