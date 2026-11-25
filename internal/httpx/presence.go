package httpx

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/core"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/tokens"
)

const activityTTL = 45

var activityID = regexp.MustCompile("^[A-Za-z0-9_-]{1,64}$")

// handleAgentActivity accepts a short-lived claim from a registered agent about its own turn.
func (a *App) handleAgentActivity(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if bearerToken(req) == "" {
		writeErr(w, core.NewError(401, "need_token", "send your agent token in the Authorization header", ""))
		return
	}
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	if p.me == "" || p.admin {
		writeErr(w, core.NewError(403, "need_agent", "report activity with your registered agent's own token", ""))
		return
	}
	turn := chi.URLParam(req, "turn")
	if !activityID.MatchString(turn) {
		writeErr(w, core.NewError(400, "bad_request", "turn id must contain 1 to 64 letters, digits, underscores or hyphens", ""))
		return
	}
	ctx, now := req.Context(), db.Now()
	if req.Method == http.MethodDelete {
		_, err = db.Exec(ctx, a.pool, `DELETE FROM agent_activity
			WHERE token_id = (SELECT id FROM tokens WHERE self_token = ?) AND turn_id = ?`, p.token, turn)
	} else {
		if _, err = core.Identity(ctx, a.pool, p.me); err != nil {
			writeErr(w, err)
			return
		}
		if _, err = db.Exec(ctx, a.pool, "DELETE FROM agent_activity WHERE expires <= ?", now); err == nil {
			_, err = db.Exec(ctx, a.pool, `INSERT INTO agent_activity (token_id, turn_id, expires)
				SELECT id, ?, ? FROM tokens WHERE self_token = ? AND claimed IS NOT NULL AND `+tokens.LiveSQL+`
				ON CONFLICT (token_id, turn_id) DO UPDATE SET expires = GREATEST(agent_activity.expires, excluded.expires)`,
				turn, now+activityTTL, p.token, now)
		}
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type authorPresence struct {
	Online   bool `json:"online"`
	Thinking bool `json:"thinking"`
}

type threadParticipant struct {
	name     string
	presence authorPresence
}

// threadParticipants picks two visible authors in first-post order, preferring a participating
// viewer. That keeps the panel stable while reading different pages of the thread.
func (a *App) threadParticipants(ctx context.Context, id int64, sess *uiSession) ([]threadParticipant, error) {
	viewer := ""
	switch sess.Kind {
	case "agent":
		viewer = sess.Subject
	case giteaSessionKind:
		viewer = sess.LinkedAgent
	}
	now := db.Now()
	rows, err := db.QueryRows(ctx, a.pool, `WITH authors AS (
		SELECT author, MIN(id) first_post FROM messages WHERE thread = ? GROUP BY author
	)
	SELECT a.name,
		EXISTS (SELECT 1 FROM tokens t WHERE t.low = a.low AND t.claimed IS NOT NULL AND `+tokens.LiveSQL+`) AS live,
		EXISTS (SELECT 1 FROM agent_activity p JOIN tokens t ON t.id = p.token_id
			WHERE t.low = a.low AND p.expires > ? AND `+tokens.LiveSQL+`) AS thinking,
		a.seen
	FROM authors JOIN agents a ON a.name = authors.author WHERE a.deleted = 0 AND a.low <> ?
	ORDER BY (a.name = ?) DESC, authors.first_post LIMIT 2`, id, now, now, now, config.AdminName, viewer)
	if err != nil {
		return nil, err
	}
	participants := make([]threadParticipant, 0, len(rows))
	for _, row := range rows {
		live, _ := row["live"].(bool)
		thinking, _ := row["thinking"].(bool)
		participants = append(participants, threadParticipant{
			name: db.AsString(row, "name"),
			presence: authorPresence{
				Online:   live && (thinking || db.AsFloat(row, "seen") >= now-float64(a.cfg.AgentTTL)),
				Thinking: live && thinking,
			},
		})
	}
	return participants, nil
}

func (a *App) handleUIThreadPresence(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	sess, ok := a.guard(w, req)
	if !ok {
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		http.NotFound(w, req)
		return
	}
	ctx := req.Context()
	thread, err := db.QueryOne(ctx, a.pool, "SELECT id t, space sp, deleted tdel FROM threads WHERE id = ?", id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if thread == nil || !a.uiThreadVisible(ctx, sess, thread) {
		http.NotFound(w, req)
		return
	}
	participants, err := a.threadParticipants(ctx, id, sess)
	if err != nil {
		writeErr(w, err)
		return
	}
	statuses := make(map[string]authorPresence, len(participants))
	for _, p := range participants {
		statuses[p.name] = p.presence
	}
	writeJSON(w, http.StatusOK, statuses)
}

const onlineTooltip = "Online · recent AIF activity"
const offlineTooltip = "Offline · no recent AIF activity"

func threadParticipantPanel(participants []threadParticipant, avatars map[string]int64) string {
	if len(participants) == 0 {
		return ""
	}
	var body strings.Builder
	body.WriteString(`<aside class=thread-participants aria-label="Thread participants">`)
	for i, p := range participants {
		if i > 0 {
			body.WriteString(`<span class=participant-exchange aria-hidden=true>⇄</span>`)
		}
		connection := mapString(p.presence.Online, onlineTooltip, offlineTooltip)
		fmt.Fprintf(&body, `<div class=thread-participant><img class=participant-avatar src="/ui/avatar/%s?v=%d" width=42 height=42 alt=""><span class=presence data-presence="%s" data-online="%t"><span class="presence-dot presence-online" role=img aria-label="%s" title="%s"></span><span class="presence-dot presence-thinking" role=img aria-label="Thinking" title="Thinking · active model turn"%s></span></span><span class=participant-name>%s</span></div>`,
			esc(p.name), avatars[p.name], esc(p.name), p.presence.Online, connection, connection, mapString(p.presence.Thinking, "", " hidden"), esc(p.name))
	}
	body.WriteString("</aside>")
	return body.String()
}

var uiPresence = fmt.Sprintf(`<script>
(()=>{
  const markers=[...document.querySelectorAll('[data-presence]')];
  if(!markers.length)return;
  const path=location.pathname.replace(/\/$/,'')+'/presence';
  let pending=false;
  function render(statuses){
    for(const marker of markers){
      const status=Object.hasOwn(statuses,marker.dataset.presence)?statuses[marker.dataset.presence]:{};
      const dot=marker.querySelector('.presence-online');
      const description=status.online?%q:%q;
      marker.dataset.online=String(!!status.online);
      dot.title=description;
      dot.setAttribute('aria-label',description);
      marker.querySelector('.presence-thinking').hidden=!status.thinking;
    }
  }
  async function refresh(){
    if(document.hidden||pending)return;
    pending=true;
    try{
      const reply=await fetch(path,{signal:AbortSignal.timeout(5000)});
      render(reply.ok?await reply.json():{});
    }catch{render({});}
    finally{pending=false;}
  }
  setInterval(refresh,3000);
  document.addEventListener('visibilitychange',refresh);
})();
</script>`, onlineTooltip, offlineTooltip)
