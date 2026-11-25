package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// 上传签名材料后自动重签。
//
// 为什么需要
// ---------
// 换 Profile 的完整流程是：AGC 下载 → 上传到 CICD → 重签 → 同步蒲公英。
// 中间「重签」这一步没有任何决策 —— 只要材料齐了就必须做，忘了反而会让
// 蒲公英上的包还是旧签名。既然无法忘记，就该自动做掉。
//
// 为什么异步
// ---------
// 重签要起 JVM、读 37MB 的包、算签名，实测 10-30 秒。上传接口的响应是
// 用户盯着看的，同步做会让「上传」按钮转半分钟。
//
// 用后台 goroutine + 任务状态：接口立刻返回「已排入队列」，前端轮询状态。
// 这也顺带解决了「重签期间用户又传了一个文件」的并发问题（见 resignQueue）。

// resignTask 一次自动重签任务的状态。
type resignTask struct {
	// ID 应用 ID（bundleName）
	ID string `json:"id"`
	// Trigger 触发来源：cert / profile / keystore / hap / manual
	Trigger string `json:"trigger"`
	// State running / done / failed
	State string `json:"state"`
	// StartedAt 开始时间（unix 秒）
	StartedAt int64 `json:"started_at"`
	// FinishedAt 结束时间（0 表示仍在跑）
	FinishedAt int64 `json:"finished_at"`
	// Message 成功提示或失败原因
	Message string `json:"message,omitempty"`
	// Queued 是否有后续排队的重签请求
	Queued bool `json:"queued,omitempty"`
}

const (
	resignStateRunning = "running"
	resignStateDone    = "done"
	resignStateFailed  = "failed"
)

// resignQueue 管理每个应用的自动重签任务。
//
// 并发策略：同一个应用串行、不同应用并行。
//
// 为什么同应用必须串行：连续上传 cert 和 profile 会各触发一次重签，
// 两次同时跑会同时读写同一个 app.hap —— 其中一个必然拿到半截文件。
// 更糟的是两次签名可能都在读旧材料，最终产出的包用了哪份证书不可预期。
//
// 为什么不同应用可以并行：它们操作各自目录下的文件，没有共享状态。
// 串行化所有应用会让「同时更新两个 app」的场景白等一倍时间。
type resignQueue struct {
	mu      sync.Mutex
	running map[string]bool   // 正在跑的应用
	pending map[string]string // 排队中的触发来源（后到的覆盖先到的）
	tasks   map[string]*resignTask
}

func newResignQueue() *resignQueue {
	return &resignQueue{
		running: map[string]bool{},
		pending: map[string]string{},
		tasks:   map[string]*resignTask{},
	}
}

// trigger 请求对某应用发起自动重签。
//
// 返回当前任务快照。若已有任务在跑，本次请求排队并在返回中标记 Queued。
func (q *resignQueue) trigger(id, source string) resignTask {
	q.mu.Lock()

	// 已有任务在跑 → 记下「待跑一次」，不重复起 goroutine
	if q.running[id] {
		q.pending[id] = source
		snap := resignTask{}
		if t, ok := q.tasks[id]; ok {
			snap = *t
		}
		snap.Queued = true
		q.mu.Unlock()
		return snap
	}

	q.running[id] = true
	task := &resignTask{
		ID:        id,
		Trigger:   source,
		State:     resignStateRunning,
		StartedAt: time.Now().Unix(),
	}
	q.tasks[id] = task
	snap := *task
	q.mu.Unlock()
	return snap
}

// finish 结束当前任务，并检查是否有排队请求需要接着跑。
//
// 返回 true 表示还有一次要跑（调用方应再执行一轮）。
func (q *resignQueue) finish(id string, state, message string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if t, ok := q.tasks[id]; ok {
		t.State = state
		t.FinishedAt = time.Now().Unix()
		t.Message = message
	}
	q.running[id] = false

	if next, ok := q.pending[id]; ok {
		delete(q.pending, id)
		q.running[id] = true
		q.tasks[id] = &resignTask{
			ID:        id,
			Trigger:   next,
			State:     resignStateRunning,
			StartedAt: time.Now().Unix(),
		}
		return true
	}
	return false
}

// status 取任务快照。没有任务时返回零值（State 为空）。
func (q *resignQueue) status(id string) resignTask {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t, ok := q.tasks[id]; ok {
		snap := *t
		snap.Queued = q.pending[id] != ""
		return snap
	}
	return resignTask{}
}

// autoResignAfterUpload 上传材料后自动重签。
//
// 只在「材料齐了」时才触发：缺 cert/profile/keystore 或 HAP 不存在时直接
// 跳过 —— 那种情况下重签必然失败，报错只会干扰用户（他还在传剩下的文件）。
func (s *server) autoResignAfterUpload(bundle, kind string) {
	// 防御：单测里 server 可能没初始化这些依赖，直接跳过而不是 panic
	if s.hmos == nil || s.resignQ == nil {
		return
	}
	app, ok := s.hmos.Get(bundle)
	if !ok {
		return
	}

	// 材料齐备性检查。用推导路径而不是 app 里存的字段 —— 上传写的是
	// <root>/<bundle>/ 下的固定文件名，两者必须一致。
	certPath, profPath, keyPath, hapPath := hmosAppPaths(s.cfg.HmosUploadDir, app.BundleName)
	for _, p := range []string{certPath, profPath, keyPath, hapPath} {
		if _, err := os.Stat(p); err != nil {
			// 还缺文件，不触发。用户传完最后一个时会自然触发。
			return
		}
	}
	// 签名工具没配就别排任务了，否则每次上传都在日志里刷「重签失败」
	if s.cfg.HmosSignTool == "" {
		return
	}

	snap := s.resignQ.trigger(bundle, kind)
	if snap.Queued {
		log.Printf("[hmos] %s 已有重签任务在跑，本次（%s）已排队", bundle, kind)
		return
	}

	log.Printf("[hmos] %s 上传了 %s，自动开始重签", bundle, kind)
	go s.runAutoResign(bundle)
}

// runAutoResign 执行自动重签，直到队列清空。
//
// 循环处理「重签期间又有新上传」的情况：每次完成后检查是否还有排队请求，
// 有就再跑一轮。这样连续上传 cert+profile 最终一定用的是最后那份材料。
func (s *server) runAutoResign(bundle string) {
	for {
		app, ok := s.hmos.Get(bundle)
		if !ok {
			s.resignQ.finish(bundle, resignStateFailed, "应用已被删除")
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), resignTimeout)
		res, err := s.resignAndReplace(ctx, app)
		cancel()

		var state, msg string
		if err != nil {
			state, msg = resignStateFailed, err.Error()
			// 记进 last_error，前端卡片会显示红字
			s.hmos.Patch(bundle, func(a *HmosApp) { a.LastError = err.Error() })
		} else {
			state = resignStateDone
			msg = "重签成功"
			// 清掉上一次的错误
			s.hmos.Patch(bundle, func(a *HmosApp) { a.LastError = "" })
			log.Printf("[hmos] %s 自动重签成功（%d 字节）", bundle, res.Size)
		}
		if saveErr := s.hmos.Save(); saveErr != nil {
			log.Printf("[hmos] 保存重签结果失败: %v", saveErr)
		}

		// finish 返回 true 表示还有排队请求，继续下一轮
		if !s.resignQ.finish(bundle, state, msg) {
			return
		}
		log.Printf("[hmos] %s 有排队的重签请求，继续执行", bundle)
		// 短暂等待，避免极端情况下连续触发把 CPU 打满
		time.Sleep(500 * time.Millisecond)
	}
}

// handleHmosResignStatus GET /api/hmos/resign-status?id=xxx
//
// 自动重签是异步的，前端需要知道「什么时候好」。轮询这个接口：
// state 从 running 变成 done/failed 就停。
//
// 不用 SSE/WebSocket：重签只跑 10-30 秒，轮询间隔 1-2 秒完全够用，
// 而 SSE 在 Cloudflare 隧道下容易被缓冲掉。
func (s *server) handleHmosResignStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("缺少 id 参数"))
		return
	}

	if s.resignQ == nil {
		// 服务未初始化（单测场景）：返回空任务而不是崩溃
		writeJSON(w, http.StatusOK, map[string]any{"task": resignTask{}, "app": nil})
		return
	}
	task := s.resignQ.status(id)

	// 任务结束时顺带把最新的应用视图带回去 —— 前端不用再发一次 /api/hmos
	var view *hmosView
	if app, ok := s.hmos.Get(id); ok {
		v := buildHmosViewWithKey(app, s.cfg.HmosWarnDays, s.cfg.HmosUploadDir,
			os.Getenv("HMOS_KEYSTORE_PWD"), s.cfg.HmosJava)
		view = &v
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"task": task,
		"app":  view,
	})
}
