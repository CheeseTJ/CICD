package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 自动重签队列的测试。
//
// 这里的并发语义很容易写错，而且错了很难在生产中复现：
//   - 同应用串行：连续上传 cert + profile 各触发一次，两次同时跑会
//     同时读写同一个 app.hap，其中一个必然拿到半截文件
//   - 不同应用并行：它们操作各自目录，串行化只会白白多等一倍时间

func TestResignQueueTriggerStartsTask(t *testing.T) {
	q := newResignQueue()
	snap := q.trigger("com.a", "profile")

	if snap.State != resignStateRunning {
		t.Errorf("State = %q, 期望 running", snap.State)
	}
	if snap.Queued {
		t.Error("第一次触发不该标记为排队")
	}
	if snap.StartedAt == 0 {
		t.Error("应记录开始时间")
	}
}

// 同一应用第二次触发要排队，不能起第二个 goroutine。
func TestResignQueueSerializesSameApp(t *testing.T) {
	q := newResignQueue()
	q.trigger("com.a", "cert")

	second := q.trigger("com.a", "profile")
	if !second.Queued {
		t.Error("同应用第二次触发应标记为排队")
	}
}

// 不同应用互不阻塞。
func TestResignQueueParallelAcrossApps(t *testing.T) {
	q := newResignQueue()
	q.trigger("com.a", "cert")

	other := q.trigger("com.b", "cert")
	if other.Queued {
		t.Error("不同应用不该被排队")
	}
	if other.State != resignStateRunning {
		t.Errorf("State = %q", other.State)
	}
}

// finish 有排队请求时返回 true，让调用方再跑一轮。
func TestResignQueueFinishRunsPending(t *testing.T) {
	q := newResignQueue()
	q.trigger("com.a", "cert")
	q.trigger("com.a", "profile") // 排队

	if !q.finish("com.a", resignStateDone, "ok") {
		t.Fatal("有排队请求时应返回 true")
	}
	// 排队任务已转为运行中
	snap := q.status("com.a")
	if snap.State != resignStateRunning {
		t.Errorf("排队任务应已启动，State = %q", snap.State)
	}
	// 再 finish 一次，队列空了应返回 false
	if q.finish("com.a", resignStateDone, "ok") {
		t.Error("队列空时应返回 false")
	}
}

func TestResignQueueFinishNoPending(t *testing.T) {
	q := newResignQueue()
	q.trigger("com.a", "cert")

	if q.finish("com.a", resignStateFailed, "boom") {
		t.Error("没有排队请求时应返回 false")
	}
	snap := q.status("com.a")
	if snap.State != resignStateFailed {
		t.Errorf("State = %q, 期望 failed", snap.State)
	}
	if snap.Message != "boom" {
		t.Errorf("Message = %q", snap.Message)
	}
	if snap.FinishedAt == 0 {
		t.Error("应记录结束时间")
	}
}

// 多个排队请求只保留最后一次的触发来源（后到覆盖先到），
// 因为重签会读最新文件，中间那次没有意义。
func TestResignQueuePendingOverwrites(t *testing.T) {
	q := newResignQueue()
	q.trigger("com.a", "cert")
	q.trigger("com.a", "keystore")
	q.trigger("com.a", "hap") // 最后一次

	if !q.finish("com.a", resignStateDone, "ok") {
		t.Fatal("应返回 true")
	}
	snap := q.status("com.a")
	if snap.Trigger != "hap" {
		t.Errorf("Trigger = %q, 期望最后到的 hap", snap.Trigger)
	}
	// 只剩这一次排队，不该再有第二次
	if q.finish("com.a", resignStateDone, "ok") {
		t.Error("排队的只有一次，第二次 finish 应返回 false")
	}
}

// 没有任务时 status 返回零值，前端据此判断「还没跑过」。
func TestResignQueueStatusEmpty(t *testing.T) {
	q := newResignQueue()
	snap := q.status("com.none")
	if snap.State != "" {
		t.Errorf("无任务时 State 应为空，实际 %q", snap.State)
	}
}

// 并发触发同一个应用时，只能有一个真正启动。
//
// 这是最容易出错的地方：如果不加锁，两个 goroutine 同时进 trigger
// 会都看到 running=false，于是起两个重签任务 → 同时写同一个 hap。
func TestResignQueueConcurrentTriggerOnlyOneStarts(t *testing.T) {
	q := newResignQueue()

	const n = 20
	var started int
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := q.trigger("com.a", "cert")
			if !snap.Queued {
				mu.Lock()
				started++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if started != 1 {
		t.Errorf("并发触发 %d 次，应只有 1 次真正启动，实际 %d", n, started)
	}
}

// ---------------------------------------------------------------- 触发条件

// 材料不齐时不该触发：那时重签必然失败，报错只会干扰用户
// （他还在传剩下的文件）。
func TestAutoResignSkipsWhenMaterialIncomplete(t *testing.T) {
	root := t.TempDir()
	app := &HmosApp{ID: "com.x", BundleName: "com.x", Name: "X"}

	// 只写 profile，缺 cert/keystore/hap
	_, profPath, _, _ := hmosAppPaths(root, "com.x")
	if err := os.MkdirAll(filepath.Dir(profPath), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(profPath, buildProfileBytes(t, "com.x", 365), 0o600)

	st := newTestHmosStore(t, app)
	s := &server{
		cfg:     &config{HmosUploadDir: root, HmosWarnDays: 30},
		hmos:    st,
		resignQ: newResignQueue(),
	}
	s.autoResignAfterUpload("com.x", "profile")

	if snap := s.resignQ.status("com.x"); snap.State != "" {
		t.Errorf("材料不齐不该触发重签，实际 State=%q", snap.State)
	}
}

// 签名工具没配时不排队 —— 否则每次上传都在日志里刷「重签失败」。
func TestAutoResignSkipsWithoutSignTool(t *testing.T) {
	root := t.TempDir()
	app := &HmosApp{ID: "com.x", BundleName: "com.x", Name: "X"}

	certPath, profPath, keyPath, hapPath := hmosAppPaths(root, "com.x")
	os.MkdirAll(filepath.Dir(certPath), 0o755)
	os.WriteFile(certPath, []byte("CERT"), 0o600)
	os.WriteFile(profPath, buildProfileBytes(t, "com.x", 365), 0o600)
	os.WriteFile(keyPath, []byte("KEY"), 0o600)
	writeTestHap(t, hapPath, "com.x")

	st := newTestHmosStore(t, app)
	s := &server{
		cfg:     &config{HmosUploadDir: root, HmosWarnDays: 30, HmosSignTool: ""},
		hmos:    st,
		resignQ: newResignQueue(),
	}
	s.autoResignAfterUpload("com.x", "profile")

	if snap := s.resignQ.status("com.x"); snap.State != "" {
		t.Errorf("未配置签名工具时不该触发，实际 State=%q", snap.State)
	}
}

// 应用不存在时静默返回，不 panic。
func TestAutoResignUnknownApp(t *testing.T) {
	root := t.TempDir()
	st := newTestHmosStore(t)
	s := &server{
		cfg:     &config{HmosUploadDir: root, HmosWarnDays: 30},
		hmos:    st,
		resignQ: newResignQueue(),
	}
	s.autoResignAfterUpload("com.nonexistent", "profile")
	if snap := s.resignQ.status("com.nonexistent"); snap.State != "" {
		t.Errorf("未知应用不该触发，实际 State=%q", snap.State)
	}
}

// 依赖为空时（单测里常见的 server 构造）不能崩。
func TestAutoResignNilDependencies(t *testing.T) {
	s := &server{}
	s.autoResignAfterUpload("com.x", "cert") // 不应 panic
}

// 材料齐 + 工具已配时应该排入任务。
func TestAutoResignTriggersWhenReady(t *testing.T) {
	root := t.TempDir()
	app := &HmosApp{ID: "com.x", BundleName: "com.x", Name: "X"}

	certPath, profPath, keyPath, hapPath := hmosAppPaths(root, "com.x")
	os.MkdirAll(filepath.Dir(certPath), 0o755)
	os.WriteFile(certPath, []byte("CERT"), 0o600)
	os.WriteFile(profPath, buildProfileBytes(t, "com.x", 365), 0o600)
	os.WriteFile(keyPath, []byte("KEY"), 0o600)
	writeTestHap(t, hapPath, "com.x")

	// 假签名工具：文件存在即可，测试只验证「排队了」
	toolPath := filepath.Join(root, "fake-tool.jar")
	os.WriteFile(toolPath, []byte("jar"), 0o600)

	st := newTestHmosStore(t, app)
	s := &server{
		cfg:     &config{HmosUploadDir: root, HmosWarnDays: 30, HmosSignTool: toolPath},
		hmos:    st,
		resignQ: newResignQueue(),
	}

	// 注意：这里会真的起 goroutine 跑 resignAndReplace（用假 jar 必然失败），
	// 但我们要断言的只是「任务被排入了」。等一小会儿让它跑完，
	// 避免 goroutine 泄漏到其它测试。
	s.autoResignAfterUpload("com.x", "profile")

	snap := s.resignQ.status("com.x")
	if snap.State == "" {
		t.Fatal("材料齐备时应排入重签任务")
	}
	if snap.Trigger != "profile" {
		t.Errorf("Trigger = %q", snap.Trigger)
	}

	// 等后台 goroutine 收尾，避免测试间互相干扰
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.resignQ.status("com.x"); st.State != resignStateRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}
