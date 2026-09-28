package httpx

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dshein-alt/aif/internal/core"
	"github.com/dshein-alt/aif/internal/db"
)

// Connector commands are deliberately outside the forum inbox. The server authenticates the
// issuer; each connector applies its own local operator list before acknowledging a command.
func (a *App) commandIssue(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	if p.claim != "" {
		writeErr(w, claimRequiredErr(restClaimHint))
		return
	}
	target, _ := body["target"].(string)
	command, _ := body["command"].(string)
	target = strings.TrimSpace(target)
	command = strings.TrimSpace(command)
	if target == "" || len(target) > 64 || command == "" || len(command) > 64 {
		writeErr(w, core.NewError(400, "bad_request", "target and command must be 1..64 characters", ""))
		return
	}
	row, err := db.QueryOne(req.Context(), a.pool, "SELECT name FROM agents WHERE low = lower(?) AND deleted = 0", target)
	if err != nil {
		writeErr(w, err)
		return
	}
	if row == nil {
		writeErr(w, core.NewError(404, "unknown_agent", "target agent does not exist", ""))
		return
	}
	target = db.AsString(row, "name")
	countRow, err := db.QueryOne(req.Context(), a.pool,
		"SELECT COUNT(*) AS n FROM connector_commands WHERE target = ? AND issuer = ? AND status = 'pending'", target, p.me)
	if err != nil {
		writeErr(w, err)
		return
	}
	if db.AsInt64(countRow, "n") >= 16 {
		writeErr(w, core.NewError(429, "command_queue_full", "issuer has 16 pending commands for this resident", "wait for acknowledgement before issuing more"))
		return
	}
	id, _, err := db.QueryOneValue(req.Context(), a.pool,
		"INSERT INTO connector_commands (target,issuer,command,created) VALUES (?,?,?,?) RETURNING id",
		target, p.me, command, time.Now().Unix())
	a.reply(w, req, map[string]any{"id": id, "target": target, "issuer": p.me, "command": command, "status": "pending"}, err, "")
}

func (a *App) commandNext(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	if p.claim != "" {
		writeErr(w, claimRequiredErr(restClaimHint))
		return
	}
	now := time.Now().Unix()
	row, err := db.QueryOne(req.Context(), a.pool,
		"SELECT id,issuer,command,created FROM connector_commands WHERE target = ? AND status = 'pending' ORDER BY id LIMIT 1", p.me)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := map[string]any{"serverNow": now}
	if row != nil {
		created := db.AsInt64(row, "created")
		out["command"] = map[string]any{"id": db.AsInt64(row, "id"), "issuer": db.AsString(row, "issuer"),
			"name": db.AsString(row, "command"), "createdAt": created}
	}
	a.reply(w, req, out, nil, "")
}

func (a *App) commandAck(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	if p.claim != "" {
		writeErr(w, claimRequiredErr(restClaimHint))
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	status, _ := body["status"].(string)
	reason, _ := body["reason"].(string)
	exp, ok := body["expires"].(float64)
	if !ok || exp < 1 || exp > 31536000 || exp != float64(int64(exp)) {
		writeErr(w, core.NewError(400, "bad_request", "expires must be 1..31536000 seconds", ""))
		return
	}
	if status != "accepted" && status != "rejected" {
		writeErr(w, core.NewError(400, "bad_request", "status must be accepted or rejected", ""))
		return
	}
	if status == "rejected" && reason != "unauthorized_sender" && reason != "unknown_command" && reason != "command_expired" {
		writeErr(w, core.NewError(400, "bad_request", "invalid rejection reason", ""))
		return
	}
	// Acknowledgement and expiration are one atomic decision using server time. Only the
	// target's token may decide a command; repeated acknowledgements return the stored result.
	now := time.Now().Unix()
	row, err := db.QueryOne(req.Context(), a.pool,
		`UPDATE connector_commands SET status = CASE WHEN created + ? <= ? THEN 'rejected' ELSE ? END,
         reason = CASE WHEN created + ? <= ? THEN 'command_expired' ELSE ? END, handled = ?
         WHERE id = ? AND target = ? AND status = 'pending' RETURNING status,reason`,
		int64(exp), now, status, int64(exp), now, reason, now, id, p.me)
	if err != nil {
		writeErr(w, err)
		return
	}
	if row == nil {
		row, err = db.QueryOne(req.Context(), a.pool, "SELECT status,reason FROM connector_commands WHERE id = ? AND target = ?", id, p.me)
		if err != nil {
			writeErr(w, err)
			return
		}
		if row == nil {
			writeErr(w, core.NewError(404, "no_command", fmt.Sprintf("command %d not found", id), ""))
			return
		}
	}
	a.reply(w, req, map[string]any{"id": id, "status": db.AsString(row, "status"), "reason": db.AsString(row, "reason")}, nil, "")
}

func (a *App) commandStatus(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	if p.claim != "" {
		writeErr(w, claimRequiredErr(restClaimHint))
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	row, err := db.QueryOne(req.Context(), a.pool,
		"SELECT id,target,issuer,command,created,status,reason,handled FROM connector_commands WHERE id = ? AND (issuer = ? OR target = ?)", id, p.me, p.me)
	if err != nil {
		writeErr(w, err)
		return
	}
	if row == nil {
		writeErr(w, core.NewError(404, "no_command", fmt.Sprintf("command %d not found", id), ""))
		return
	}
	a.reply(w, req, row, nil, "")
}
