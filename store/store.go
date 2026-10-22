// Package store 提供任务状态的内存存储。
// 所有方法都可以被多个 goroutine 并发调用。
//
// 已知限制：状态只存在进程内存中，进程重启会丢失全部任务。
package store

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
)

const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// ErrNotFound 表示指定 ID 的任务不存在。
// 调用方应当用 errors.Is 判断，而不是比较字符串。
var ErrNotFound = errors.New("task not found")

// ErrMsgServerRestart 是崩溃恢复时为「僵尸 running」写入的终态文案，
// 与优雅停机的 "server shutdown" 区分：它代表上次进程非正常死亡。
const ErrMsgServerRestart = "server restarted unexpectedly"

// TaskStore 是任务存储的抽象接口：内存 Store 与 SQLite SQLStore 都实现它。
// 用接口而非具体类型，是为了让 main 按配置在两种后端间切换
// （ADR-0010：SQLite 持久化可插拔，默认内存）。
type TaskStore interface {
	Create(prompt string) (id string)
	Update(id, status string) error
	Get(id string) (Task, error)
	Complete(id, result string, err error) error
	// Delete 移除指定 id 的任务。id 不存在时返回 ErrNotFound，
	// 与 Update/Complete 的语义一致——不存在的 id 不被静默忽略。
	Delete(id string) error
	// RecoverTasks 启动补偿：pending 按提交顺序返回以便重新入队，
	// 僵尸 running（进程非正常死亡的残留）批量标记为 failed。
	// 返回的 recoveredRunning 是被标记失败的任务数。
	RecoverTasks(ctx context.Context) (pendingIDs []string, recoveredRunning int, err error)
}

// Store 是任务的并发安全容器。
//
// map 本身不支持并发写（会直接 panic），且 Update/Complete 都是
// 读-改-写的复合操作，必须整段加锁才不会丢更新。
type Store struct {
	tasks  map[string]Task
	mu     sync.Mutex
	nextID int
}

// Task 是一个异步任务的全部状态。
//
// 注意：整个结构体会被 api 层序列化后返回给调用方，
// 因此不能往任何字段里放上游返回体、密钥等内部信息。
//
// json tag 必须显式声明：不加 tag 时 encoding/json 会直接用 Go 字段名
// （ID/Status/...，首字母大写），前端按小写字段读就会拿到 undefined。
// 序列化的字段名是对外契约，由 tag 固定，改字段名不会静默破坏调用方。
type Task struct {
	ID     string `json:"id"`
	Status string `json:"status"` // pending | running | done | failed
	Prompt string `json:"prompt"`
	Result string `json:"result"` // 仅 done 时有值
	Error  string `json:"error"`  // 仅 failed 时有值，且必须是脱敏后的粗粒度分类
}

// NewStore 返回一个可用的空 Store。
// 必须用它构造——零值 Store 的 map 为 nil，写入会 panic。
func NewStore() *Store {
	var store Store
	store.tasks = make(map[string]Task)
	return &store
}

// Create 登记一个新任务并返回其 ID，初始状态为 pending。
func (s *Store) Create(prompt string) (id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 自增计数器保证 ID 唯一；用 prompt 作键会让重复提交互相覆盖
	s.nextID++
	id = strconv.Itoa(s.nextID)

	s.tasks[id] = Task{
		ID:     id,
		Prompt: prompt,
		Status: StatusPending,
	}

	return id
}

// Update 只迁移状态，不触碰 Result 和 Error。
// 任务不存在时返回 ErrNotFound——不存在的 ID 不能被静默创建出来。
func (s *Store) Update(id string, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrNotFound
	}
	// 读-改-写：map 里存的是值，必须整个写回才生效
	task.Status = status
	s.tasks[id] = task

	return nil
}

// Complete 写入任务的终态。err 为 nil 记为 done，否则记为 failed。
//
// 由 Store 自己判断终态，调用方就无法拼出「done 却带着错误」这类
// 自相矛盾的组合——状态与数据的一致性规则只存在这一处。
//
// 返回的 error 描述的是存储操作本身是否成功，与 err 参数无关：
// 任务失败但成功记录下来，返回的仍是 nil。
func (s *Store) Complete(ID string, result string, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[ID]
	if !ok {
		return ErrNotFound
	}

	if err != nil {
		task.Status = StatusFailed
		task.Error = err.Error()
		s.tasks[ID] = task

		return nil
	}

	task.Status = StatusDone
	task.Result = result

	s.tasks[ID] = task

	return nil
}

// Get 返回任务的副本。
// 返回副本而非指针，调用方改动不会污染内部状态，
// 从设计上消除了一整类数据竞争。
func (s *Store) Get(ID string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.tasks[ID]
	if !ok {
		return Task{}, ErrNotFound
	}
	return v, nil
}

// Delete 从 map 中移除指定 id 的任务。
// id 不存在时返回 ErrNotFound——与 Update/Complete 一致，不静默成功。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[id]; !ok {
		return ErrNotFound
	}
	delete(s.tasks, id)

	return nil
}

// RecoverTasks 在启动阶段修复内存状态：返回 pending 任务（按自增 ID 升序），
// 并把残留的 running（仅可能来自进程重启后未清理）标定为失败。
// 内存实现本身重启即丢失，此方法主要为与 SQLStore 保持同一接口语义。
func (s *Store) RecoverTasks(ctx context.Context) ([]string, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var pending []string
	recovered := 0
	for id, task := range s.tasks {
		switch task.Status {
		case StatusPending:
			pending = append(pending, id)
		case StatusRunning:
			task.Status = StatusFailed
			task.Error = ErrMsgServerRestart
			s.tasks[id] = task
			recovered++
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		a, errA := strconv.Atoi(pending[i])
		b, errB := strconv.Atoi(pending[j])
		if errA != nil || errB != nil {
			return pending[i] < pending[j]
		}
		return a < b
	})

	return pending, recovered, nil
}
