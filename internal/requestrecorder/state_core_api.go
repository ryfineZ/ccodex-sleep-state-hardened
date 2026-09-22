package requestrecorder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

func coreErrorCode(err error) string {
	var core *coreError
	if errors.As(err, &core) {
		return core.Code
	}
	if errors.Is(err, context.Canceled) {
		return "state_core_cancelled"
	}
	return "state_core_request_failed"
}

// ConfigureCore never reads a login file or clears credential stops/budgets.
func (e *Engine) ConfigureCore(p CorePolicy) error {
	if e.setup != nil {
		if !e.setup.mu.TryRLock() {
			return errors.New("连接正在变更，请稍后再试")
		}
		defer e.setup.mu.RUnlock()
		if p.Enabled {
			if err := e.setup.admit(); err != nil {
				return errors.New("请先接入 Codex，再开启核心采集注入")
			}
		}
	}
	if !e.connectionMu.TryLock() {
		return errors.New("连接正在变更，请稍后再试")
	}
	defer e.connectionMu.Unlock()
	return e.core.configure(p, e.target, e.connectionProxy, e.transport, e.config)
}
func (e *Engine) coreAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		localError(w, 405, "method_not_allowed")
		return
	}
	switch r.URL.Path {
	case "/__recorder/api/state-core/configure":
		v := struct {
			CorePolicy
			ConfirmCost bool `json:"confirm_cost"`
		}{CorePolicy: e.core.options()}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768))
		d.DisallowUnknownFields()
		if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF {
			localError(w, 400, "state_core_settings_invalid")
			return
		}
		if v.Enabled && !v.ConfirmCost {
			responseJSON(w, 400, map[string]string{"message": "开启会发送有限短采集请求，可能消耗额度；请确认后开启。跨 turn 复用属于实验功能，不保证上游继续接受。"})
			return
		}
		if err := e.ConfigureCore(v.CorePolicy); err != nil {
			responseJSON(w, 409, map[string]string{"message": err.Error()})
			return
		}
		responseJSON(w, 200, map[string]any{"state_core": e.core.status(), "persisted": false})
	case "/__recorder/api/state-core/refresh":
		var v struct {
			ID          string `json:"id"`
			ConfirmCost bool   `json:"confirm_cost"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		d.DisallowUnknownFields()
		if d.Decode(&v) != nil || v.ID == "" || !v.ConfirmCost || d.Decode(new(any)) != io.EOF {
			localError(w, 400, "state_core_refresh_confirmation_required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
		defer cancel()
		if err := e.core.refresh(ctx, v.ID); err != nil {
			responseJSON(w, 409, map[string]string{"message": coreErrorCode(err)})
			return
		}
		responseJSON(w, 200, e.core.status())
	default:
		localError(w, 404, "state_core_unknown_action")
	}
}
