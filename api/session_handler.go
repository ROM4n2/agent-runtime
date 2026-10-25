package api

import (
	"agent-runtime/store"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
)

// HandleGetSession 查询或自动初始化会话元数据（GET /sessions/{id}）。
//
// 语义遵循 GetOrCreate：会话不存在时透明创建空会话，简化客户端首次交互握手流程。
// 必须依赖底座存储实现 store.SessionStore 契约，未支持时明确返回 501。
func (h *Handler) HandleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id is empty", http.StatusBadRequest)
		return
	}

	ss, ok := h.store.(store.SessionStore)
	if !ok {
		// 底座 Store 未接入会话能力时显式 501，阻断静默失败与数据丢失。
		http.Error(w, "session store not supported", http.StatusNotImplemented)
		return
	}

	sess, err := ss.GetOrCreateSession(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(sess); err != nil {
		slog.Error("encode session response", "session_id", id, "error", err)
	}
}

// HandleGetSessionMessages 拉取指定会话的历史消息流（GET /sessions/{id}/messages?limit=N）。
//
// 消息默认按 ASC（时序由先到后）返回；若提供 limit 参数且 > 0，则仅取最新的 limit 条。
// 会话暂无消息时强制初始化为空切片 []，防止 JSON 编码输出 null 导致前端解包异常。
func (h *Handler) HandleGetSessionMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id is empty", http.StatusBadRequest)
		return
	}

	ss, ok := h.store.(store.SessionStore)
	if !ok {
		http.Error(w, "session store not supported", http.StatusNotImplemented)
		return
	}

	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}

	msgs, err := ss.GetSessionMessages(r.Context(), id, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 防御性保底：nil 切片序列化为 null，必须赋为空切片以输出 [] 保证前端契约。
	if msgs == nil {
		msgs = []store.SessionMessage{}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(msgs); err != nil {
		slog.Error("encode session messages response", "session_id", id, "error", err)
	}
}
