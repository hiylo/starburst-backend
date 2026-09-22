package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hiylo/starburst-backend/internal/opencode"
	"github.com/hiylo/starburst-backend/internal/push"
	"github.com/hiylo/starburst-backend/internal/store"
)

// toolFilePartRE 匹配 part 对象里的 `"type": "tool"` / `"type":"file"`（容忍空格），
// 用于高频 message.part.updated 里挑出有动作语义的工具/文件 part 入库。
var toolFilePartRE = regexp.MustCompile(`"type"\s*:\s*"(tool|file)"`)

// eventRetention is how long captured session events are kept before the
// janitor deletes them. The App dashboard only shows "recent session
// activity", so a rolling 7-day window keeps the table small.
const eventRetention = 7 * 24 * time.Hour

// eventCleanupInterval is how often stale session events are purged.
const eventCleanupInterval = time.Hour

// eventQueueSize is the buffered channel capacity between the SSE read loop
// and the batch flusher. When full, new events are dropped (and logged)
// rather than blocking the upstream stream.
const eventQueueSize = 512

// eventFlushBatch is the maximum number of queued events written per INSERT.
const eventFlushBatch = 100

// eventFlushInterval is how often the flusher writes a non-empty partial batch.
const eventFlushInterval = 200 * time.Millisecond

// StartEventCollector runs the resident global event collector: it subscribes
// to the upstream OpenCode global SSE stream, persists every event into
// session_events, and fans non-streaming events out to the push hub for live
// /api/ws delivery. It blocks until ctx is canceled, reconnecting upstream
// with the same bounded backoff used by the per-request /api/stream relay.
//
// Persistence is decoupled from the SSE read loop: the callback only enqueues
// events into a buffered channel, and a dedicated flusher goroutine writes
// them in multi-row batches (every eventFlushInterval or eventFlushBatch
// events, whichever comes first). This keeps high-frequency delta/progress
// bursts from issuing one INSERT per event or stalling the stream.
func (s *Server) StartEventCollector(ctx context.Context) {
	// Startup sweep: drop stale events before new ones accumulate.
	s.cleanupEvents(ctx)
	go s.cleanupEventsLoop(ctx)

	// 重启后先从 PG 最近事件预填充状态表，避免丢失重启前聚合到的会话状态。
	s.seedSessionStatuses(ctx)

	// 高频 delta/progress 事件现状就是入库的（仅不广播），改造后保持入库，
	// 只是从逐条 INSERT 改为批量 INSERT。
	queue := make(chan *store.SessionEvent, eventQueueSize)
	flushed := make(chan struct{})
	go s.flushEventsLoop(ctx, queue, flushed)
	defer func() { <-flushed }() // 退出前确保残余缓冲已落库

	backoff := time.Second
	for {
		err := s.openCode.StreamEvents(ctx, func(ev opencode.SSEEvent) error {
			// 单条事件回调内任何 panic 都不该让采集器整体退出（否则推送永久静默停）。
			defer func() {
				if r := recover(); r != nil {
					log.Printf("events: collector callback panic: %v", r)
				}
			}()
			// 高频 delta/progress 事件占流量 90%+，只用来维护会话活跃心跳。
			// 先做轻量解析（只取 type+sessionID，不物化大 payload、几乎零分配），
			// 命中高频则直接返回，避免完整 json.Unmarshal 触发 GC 抖动。
			eventType, sessionID, ok := lightParseEvent(ev)
			if !ok {
				return nil
			}
			if isHighFrequencyEvent(eventType) {
				if sessionID != "" && isMessageActivityEvent(eventType) {
					s.sessionActivity.Store(sessionID, time.Now())
				}
				// message.part.updated 本按高频丢弃（文本/推理 part 每 token 一次）；
				// 但「工具」与「文件」part 的状态变化是低频、有动作语义的（运行/完成/失败
				// 某工具、产出某文件），值得入库供实时动态栏展示。用有界前缀匹配
				// 避免为整段流式文本做 O(n²) 扫描：只查前 4KB（type 字段在事件体开头），
				// 容忍 `"type" : "tool"` 这类带空格的形态，命中即完整解析落库。
				if eventType == "message.part.updated" {
					data := ev.Data
					if len(data) > 4096 {
						data = data[:4096]
					}
					if toolFilePartRE.Match(data) {
						if se, ok := parseSessionEvent(ev); ok && se.SessionID != "" {
							select {
							case queue <- se:
								s.pushSessionEvent(se)
							default:
							}
						}
					}
				}
				return nil
			}
			se, ok := parseSessionEvent(ev)
			if !ok {
				return nil
			}
			// 维护「最新会话状态」聚合表（比上游 /session/status 快照更全）。
			// session.idle 事件自身不带 status 字段，需显式置 idle。
			switch se.EventType {
			case "session.status":
				if st := parseStatusFromPayload(se.Payload); st != "" {
					s.sessionStatuses.Store(se.SessionID, st)
				}
			case "session.idle":
				s.sessionStatuses.Store(se.SessionID, "idle")
			}
			// 记录消息类活动时间：用于判定「状态已标 idle 但仍在输出」的处理中会话。
			if isMessageActivityEvent(se.EventType) {
				s.sessionActivity.Store(se.SessionID, time.Now())
			}
			select {
			case queue <- se:
				// 仅在成功入队（最终会落库）时才广播给 WS 客户端，避免「面板有、历史没有」。
				s.pushSessionEvent(se)
			default:
				// 队列满说明落库跟不上事件速率；丢弃比阻塞 SSE 读循环安全，
				// 事件流本就是可再拉取的遥测数据。丢弃事件同样不广播。
				log.Printf("events: queue full, dropping %s for session %s", se.EventType, se.SessionID)
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("events: upstream event stream dropped: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		} else {
			backoff = time.Second
		}
	}
}

// flushEventsLoop drains queued events and writes them to the store in
// batches: a flush happens when eventFlushBatch events have accumulated or
// every eventFlushInterval, whichever comes first. On ctx cancellation it
// drains the remaining queue, flushes once more, then closes flushed so the
// collector can wait for the residual buffer to land before the process exits.
func (s *Server) flushEventsLoop(ctx context.Context, queue <-chan *store.SessionEvent, flushed chan<- struct{}) {
	defer close(flushed)
	ticker := time.NewTicker(eventFlushInterval)
	defer ticker.Stop()
	batch := make([]*store.SessionEvent, 0, eventFlushBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// 落库用带超时的独立 ctx，避免采集器 ctx 取消时残余批次被丢。
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.store.InsertEvents(wctx, batch); err != nil {
			// A failed batch must not kill the stream; log and move on.
			log.Printf("events: batch insert %d events: %v", len(batch), err)
		}
		cancel()
		// 批内含新消息/待决问题/完成等事件 → 把对应会话标记为「有未读」，
		// 供 Web/App 的未读绿点展示；已读由任一端 POST /api/unread/{id} 清除。
		unreadIDs := make([]string, 0, len(batch))
		seen := make(map[string]bool)
		for _, e := range batch {
			if isUnreadTriggerEvent(e.EventType) && e.SessionID != "" && !seen[e.SessionID] {
				seen[e.SessionID] = true
				unreadIDs = append(unreadIDs, e.SessionID)
			}
		}
		if len(unreadIDs) > 0 {
			wctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
			if err := s.store.SetSessionsUnread(wctx2, unreadIDs); err != nil {
				log.Printf("events: mark session unread: %v", err)
			}
			cancel2()
		}
		batch = batch[:0]
	}
	for {
		select {
		case se := <-queue:
			batch = append(batch, se)
			if len(batch) >= eventFlushBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			// 排空队列中残余事件后最后 flush 一次。
			for {
				select {
				case se := <-queue:
					batch = append(batch, se)
					if len(batch) >= eventFlushBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// cleanupEventsLoop purges events past the retention window every hour until
// ctx is canceled.
func (s *Server) cleanupEventsLoop(ctx context.Context) {
	ticker := time.NewTicker(eventCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanupEvents(ctx)
		}
	}
}

// cleanupEvents deletes session events older than the retention window.
func (s *Server) cleanupEvents(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-eventRetention)
	n, err := s.store.DeleteEventsOlderThan(ctx, cutoff)
	if err != nil {
		log.Printf("events: retention cleanup: %v", err)
		return
	}
	if n > 0 {
		log.Printf("events: purged %d stale session events", n)
	}
}

// parseSessionEvent extracts the fields needed for persistence from a raw SSE
// event. session_id is read from the event body (v1 properties / v2 data
// shapes, both "sessionID"/"sessionId" spellings); event_type comes from the
// top-level "type" field, falling back to "unknown". Returns ok=false only for
// frames that are not valid JSON at all.
func parseSessionEvent(ev opencode.SSEEvent) (*store.SessionEvent, bool) {
	var body struct {
		Type    string         `json:"type"`
		Props   map[string]any `json:"properties"`
		Data    map[string]any `json:"data"`
		Session string         `json:"sessionID"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(ev.Data, &body); err != nil {
		return nil, false
	}
	eventType := strings.TrimSpace(body.Type)
	if eventType == "" {
		// opencode wraps the event in a top-level "payload" object (v1.18+):
		// {"payload":{"id":..,"type":"message.part.delta","properties":{...}},"project":..,"directory":..}
		if pt, ok := body.Payload["type"].(string); ok {
			eventType = strings.TrimSpace(pt)
		}
	}
	if eventType == "" {
		eventType = "unknown"
	}
	sessionID := body.Session
	if sessionID == "" {
		sessionID = lookupSessionID(body.Data)
	}
	if sessionID == "" {
		sessionID = lookupSessionID(body.Props)
	}
	if sessionID == "" {
		// v1.18+ wrapper: the session id lives in payload.properties/data.
		sessionID = lookupSessionID(payloadObject(body.Payload))
	}
	return &store.SessionEvent{
		SessionID: sessionID,
		EventType: eventType,
		Payload:   append([]byte(nil), ev.Data...),
		CreatedAt: time.Now().UTC(),
	}, true
}

// lookupSessionID finds a session identifier in a decoded JSON object,
// tolerating both "sessionID"/"sessionId" spellings. Values that are not
// strings (e.g. the key is absent) are ignored.
func lookupSessionID(m map[string]any) string {
	for _, k := range []string{"sessionID", "sessionId"} {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// payloadObject returns the first nested object inside the v1.18+ "payload"
// wrapper that may carry the event detail ("properties" or "data"), or nil.
func payloadObject(p map[string]any) map[string]any {
	for _, k := range []string{"properties", "data"} {
		if m, ok := p[k]; ok {
			if mm, ok := m.(map[string]any); ok {
				return mm
			}
		}
	}
	return nil
}

// isHighFrequencyEvent reports whether the event is a low-level streaming
// delta. These are persisted but not broadcast, so the live /api/ws path is
// not flooded with per-chunk updates; the App already treats the same set as
// high-frequency.
// lightParseEvent 从原始 SSE 事件中只取 type 与 sessionID，不物化大的
// payload（delta 文本等），用于高频事件热路径，几乎零分配。
func lightParseEvent(ev opencode.SSEEvent) (eventType, sessionID string, ok bool) {
	// 嵌套 session 字段同时接受 sessionID/sessionId 两种拼写（对齐 lookupSessionID
	// 的宽容处理）。仍用结构体解码而非 map：只取所需字段，不物化大的 delta 文本
	// payload。v1.18 wrapper 的 session id 在 payload.properties / payload.data 里。
	type sidFields struct {
		Session    string `json:"sessionID"`
		SessionAlt string `json:"sessionId"`
	}
	var body struct {
		Type    string    `json:"type"`
		Session string    `json:"sessionID"`
		Props   sidFields `json:"properties"`
		Data    sidFields `json:"data"`
		Payload struct {
			Type  string    `json:"type"`
			Props sidFields `json:"properties"`
			Data  sidFields `json:"data"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(ev.Data, &body); err != nil {
		return "", "", false
	}
	eventType = strings.TrimSpace(body.Type)
	if eventType == "" {
		eventType = strings.TrimSpace(body.Payload.Type)
	}
	if eventType == "" {
		eventType = "unknown"
	}
	sessionID = strings.TrimSpace(body.Session)
	if sessionID == "" {
		sessionID = sessionIDFromFields(body.Props.Session, body.Props.SessionAlt)
	}
	if sessionID == "" {
		sessionID = sessionIDFromFields(body.Data.Session, body.Data.SessionAlt)
	}
	if sessionID == "" {
		sessionID = sessionIDFromFields(body.Payload.Props.Session, body.Payload.Props.SessionAlt)
	}
	if sessionID == "" {
		sessionID = sessionIDFromFields(body.Payload.Data.Session, body.Payload.Data.SessionAlt)
	}
	return eventType, sessionID, true
}

// sessionIDFromFields returns the first non-empty session id among the given
// fields, tolerating both "sessionID"/"sessionId" spellings like lookupSessionID.
func sessionIDFromFields(fields ...string) string {
	for _, f := range fields {
		if v := strings.TrimSpace(f); v != "" {
			return v
		}
	}
	return ""
}

func isHighFrequencyEvent(eventType string) bool {
	return strings.HasSuffix(eventType, ".delta") ||
		strings.Contains(eventType, "progress") ||
		eventType == "message.part.updated" ||
		eventType == "message.part.removed" ||
		eventType == "heartbeat"
}

// unwrapEventPayload 解包 wrapped v1.18 事件：App 端推送进来时事件主体被包成
// {directory,project,payload:{id,type,properties:{sessionID,...}}}，而 App 端
// container() 解析器只下钻一层（先看 payload.properties，再看 payload.data，
// 再看 payload.payload），取不到内部 properties。此处把 payload object 内的
// 事件主体抽出来作为对外推送 payload，保证 permission.asked / session.error /
// session.status 等通知能命中。若 payload 对象内没有 properties/data，则整体
// 返回该 payload 对象；解析失败或没有顶层 payload 对象时原样返回 raw。
func unwrapEventPayload(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	inner, ok := m["payload"]
	if !ok {
		return raw
	}
	var payloadObj map[string]json.RawMessage
	if err := json.Unmarshal(inner, &payloadObj); err != nil {
		return raw
	}
	for _, key := range []string{"properties", "data"} {
		if v, ok := payloadObj[key]; ok {
			var vm map[string]json.RawMessage
			if err := json.Unmarshal(v, &vm); err == nil {
				return v
			}
		}
	}
	return inner
}

// pushSessionEvent fans a captured event out to every connected /api/ws client
// so dashboards update live without polling.
func (s *Server) pushSessionEvent(se *store.SessionEvent) {
	b, err := json.Marshal(map[string]any{
		"sessionId": se.SessionID,
		"eventType": se.EventType,
		"payload":   json.RawMessage(unwrapEventPayload([]byte(se.Payload))),
	})
	if err != nil {
		return
	}
	s.hub.Broadcast(push.Message{Type: "session.event", Payload: b})
}

// handleEvents lists recorded session events for the App dashboard and the web
// AI workbench ("recent session activity"). Requires an APP token or a web
// session. Query params: since (RFC3339 or unix milliseconds, optional), limit
// (default 200, max 1000), sessionId (optional). Results are ordered by
// created_at ascending, forming a stable forward cursor.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok && !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	q := r.URL.Query()
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 1000 {
				limit = 1000
			}
		}
	}
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, ok := parseEventSince(v)
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid since: use RFC3339 or unix milliseconds")
			return
		}
		since = t
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	items, err := s.store.ListEvents(ctx, q.Get("sessionId"), since, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list events failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": items})
}

// parseEventSince parses the since filter as either an RFC3339 timestamp or
// unix milliseconds, normalizing to UTC for dialect-safe timestamp comparison.
func parseEventSince(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC(), true
	}
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC(), true
	}
	return time.Time{}, false
}

// parseStatusFromPayload extracts the session status type from a raw event body
// shaped like {"payload":{"type":"session.status","properties":{"status":{"type":"busy"}}}}.
func parseStatusFromPayload(payload []byte) string {
	var body struct {
		Payload struct {
			Properties struct {
				Status struct {
					Type string `json:"type"`
				} `json:"status"`
			} `json:"properties"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Payload.Properties.Status.Type)
}

// isMessageActivityEvent reports whether the event indicates the session is
// actively producing output (streaming deltas, message updates, completion).
func isMessageActivityEvent(eventType string) bool {
	switch eventType {
	case "message.part.delta", "message.part.updated", "message.updated", "message.complete":
		return true
	}
	return false
}

// isUnreadTriggerEvent reports whether the event should flag its session as
// having new/unread activity for the shared Web/App indicator: a completed or
// updated message, a pending question/permission, or a session turning idle /
// erroring (i.e. something happened that a user may want to look at).
func isUnreadTriggerEvent(eventType string) bool {
	switch eventType {
	case "message.complete", "message.created", "message.updated",
		"question.asked", "question.updated", "permission.asked",
		"session.idle", "session.status", "session.error", "session.failed":
		return true
	}
	return false
}

// seedSessionStatuses pre-populates the in-memory session status map from
// recent persisted events (the map is lost on restart; the events table is not).
func (s *Server) seedSessionStatuses(ctx context.Context) {
	// 用 since=time.Time{} 走 ListEvents 的 DESC 分支取「最近」的 limit 条事件，
	// 而不是传非零 cutoff（那会 ORDER BY created_at ASC 取到窗口内最旧的 1000 条，
	// 一小时事件 >1000 时当前状态全部丢失）。
	events, err := s.store.ListEvents(ctx, "", time.Time{}, 1000)
	if err != nil {
		log.Printf("events: seed session statuses: %v", err)
		return
	}
	// seed 项必须同步填 sessionActivity：否则重启后第一次聚合时所有 seed 项
	// hasActivity=false → busy 立即 Delete、idle 也立即 Delete，seed 形同虚设。
	// 用 time.Now() 让 seed 项撑过一个 stale 窗口（0 < activeWindow/busyStaleWindow），
	// 等真实事件到来接管；与 push 路径（sessionActivity.Store(id, time.Now())）
	// 保持一致的时间源语义，避免进程内短窗口比较被不同起点带偏。
	seedActivity := time.Now()
	for _, e := range events {
		switch e.EventType {
		case "session.status":
			if st := parseStatusFromPayload(e.Payload); st != "" {
				s.sessionStatuses.Store(e.SessionID, st)
				s.sessionActivity.Store(e.SessionID, seedActivity)
			}
		case "session.idle":
			s.sessionStatuses.Store(e.SessionID, "idle")
			s.sessionActivity.Store(e.SessionID, seedActivity)
		}
	}
	if n := len(events); n > 0 {
		log.Printf("events: seeded session statuses from %d recent events", n)
	}
}

// statusAggCacheTTL 是 /session/status 聚合结果的短 TTL 缓存窗口。App 高频轮询
// 时直接复用上次快照，避免每次请求都同步打上游快照 + 全表 Range 聚合。
const statusAggCacheTTL = 3 * time.Second

// statusAggEntry 是一次聚合快照的缓存条目。
type statusAggEntry struct {
	at  time.Time
	out map[string]map[string]any
}

// statusAggCache 按 Server 实例缓存：生产单实例即单条目；测试各建 Server 互不干扰，
// 不会把上一个测试服务器的快照误复用给下一个。
var statusAggCache sync.Map // *Server → *statusAggEntry

// cachedStatusAgg 返回 TTL 窗口内的聚合快照；命中时不再访问上游与聚合表。
func (s *Server) cachedStatusAgg() (map[string]map[string]any, bool) {
	v, ok := statusAggCache.Load(s)
	if !ok {
		return nil, false
	}
	entry := v.(*statusAggEntry)
	if time.Since(entry.at) < statusAggCacheTTL {
		return entry.out, true
	}
	statusAggCache.Delete(s)
	return nil, false
}

// storeStatusAgg 写入本次聚合快照供后续请求复用。
func (s *Server) storeStatusAgg(out map[string]map[string]any) {
	statusAggCache.Store(s, &statusAggEntry{at: time.Now(), out: out})
}

// handleSessionStatusAgg 是原 `/session/status` 镜像的增强：上游快照 + 采集器
// 事件聚合合并，返回最准确的会话状态视图。endpoint 不变（仍经镜像代理）。
//
// rr 为可选参数：调用方持有 *http.Request 时传入（r），上游快照请求基于
// r.Context() 派生 5s 超时上下文——客户端断开（r.Context 取消）同样会取消上游
// 请求，超时兜底防挂死；未传时退化为 context.Background()。保持可变参数是为了
// 不破坏现有调用点（opencode_proxy 与测试当前只传 w），调用方接入 r 后无需改签名。
func (s *Server) handleSessionStatusAgg(w http.ResponseWriter, rr ...*http.Request) {
	// 读 body 用超时 ctx：快照请求整体（建连+读 body）受 5s 上限约束，同时
	// r.Context 取消（客户端断开）时上游请求也随之取消，两者取先到者。
	base := context.Background()
	if len(rr) > 0 && rr[0] != nil {
		base = rr[0].Context()
	}
	ctx, cancel := context.WithTimeout(base, 5*time.Second)
	defer cancel()
	// 命中短 TTL 缓存：直接复用上次聚合结果，跳过上游快照与全表 Range。
	if out, ok := s.cachedStatusAgg(); ok {
		writeJSON(w, http.StatusOK, out)
		return
	}
	// 快照是上游当前权威状态：优先保留；事件聚合只补充快照未覆盖的会话，
	// 避免把已 idle 的会话误标成 busy，也补齐快照遗漏的 busy。
	out := map[string]map[string]any{}
	if resp, err := s.openCode.Do(ctx, http.MethodGet, "/session/status", nil, nil, nil); err == nil {
		var m map[string]map[string]any
		if derr := json.NewDecoder(resp.Body).Decode(&m); derr == nil {
			for id, v := range m {
				if t, ok := v["type"].(string); ok && t != "" {
					// 快照命中的条目整体透传：保留 type/attempt/message/next 等
					// retry 元数据，只丢弃无有效 type 的脏条目。
					out[id] = v
				}
			}
		}
		_ = resp.Body.Close()
	}
	// 上游 status 事件并不可靠：会话被标 idle 后仍可能继续流式输出。最近有消息
	// 活动的会话若状态是 idle，纠正为 busy，避免「处理中显示空闲」。
	// busy/retry 残留清除也用它：超过该窗口无任何消息输出即视为已结束。
	// activeWindow: 无消息活动超过该窗口即不再视作「进行中」。用于 idle→busy 纠正
	// 与 sessionActivity map 的淘汰（30s 覆盖普通思考/短工具调用的停顿）。
	const activeWindow = 30 * time.Second
	// busyStaleWindow: busy/retry 残留清除窗口。长推理、长工具调用可能 >30s 无任何
	// 消息活动（不是真的结束），若与 activeWindow 同值会把仍在工作的会话误 Delete。
	// 放宽到 90s：既保留对确已停止会话残留的及时清理，又容纳长任务的无消息间隔。
	// 注意：此窗口独立于 activeWindow，后者只用于 idle→busy 纠正，语义不同。
	const busyStaleWindow = 90 * time.Second
	now := time.Now()
	s.sessionStatuses.Range(func(k, v any) bool {
		id, ok := k.(string)
		if !ok {
			return true
		}
		if _, exists := out[id]; exists {
			// 上游快照权威：命中即保留快照结果，事件聚合不透出。
			return true
		}
		st, ok := v.(string)
		if !ok || st == "" {
			return true
		}
		last, hasActivity := s.sessionActivity.Load(id)
		recent := false
		if hasActivity {
			if t, ok := last.(time.Time); ok {
				recent = now.Sub(t) < busyStaleWindow
			}
		}
		switch st {
		case "idle":
			// 已 idle 且无近期活动、也不在上游快照里 → 陈旧，清除，避免 map 无界增长。
			if !recent && !hasActivity {
				s.sessionStatuses.Delete(k)
				return true
			}
			if lastT, ok := last.(time.Time); ok && now.Sub(lastT) >= time.Hour {
				s.sessionStatuses.Delete(k)
				return true
			}
			out[id] = map[string]any{"type": st}
		case "busy", "retry":
			// 上游快照未覆盖（未在 /session/status 里）说明上游已认为该会话不再忙。
			// 若事件聚合仍挂着 busy/retry，且超活动窗口无任何消息输出 → 判定已结束，
			// 清除残留，避免「会话早已 stop 却永远显示处理中」的反向误报。
			// 注意：保留上游快照偶发漏报、但确有近期消息活动的真忙会话（子 agent 场景）。
			if !recent {
				s.sessionStatuses.Delete(k)
				return true
			}
			out[id] = map[string]any{"type": st}
		default:
			out[id] = map[string]any{"type": st}
		}
		return true
	})
	// 补充：最近有消息活动但上游快照与事件聚合都没标 busy 的会话，纠正为 busy。
	s.sessionActivity.Range(func(k, v any) bool {
		id, ok := k.(string)
		if !ok {
			return true
		}
		last, ok := v.(time.Time)
		if !ok {
			return true
		}
		if now.Sub(last) > activeWindow {
			// 超出活动窗口即无意义，顺手淘汰，避免 activity map 无界增长。
			s.sessionActivity.Delete(k)
			return true
		}
		// 纠正逻辑对 map 值里的 "type" 字段判定，(缺失视为空)：
		// 快照异常矫正 idle→busy；事件聚合补的项同理。
		m, ok := out[id]
		if !ok {
			out[id] = map[string]any{"type": "busy"}
			return true
		}
		if t, _ := m["type"].(string); t == "idle" || t == "" {
			m["type"] = "busy"
		}
		return true
	})
	s.storeStatusAgg(out)
	writeJSON(w, http.StatusOK, out)
}
