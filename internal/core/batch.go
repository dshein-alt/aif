package core

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	spec(&Op{
		Name: "batch",
		Summary: "run several ops in one call; results come back in order, an error does not roll back the others",
		Params: map[string]string{
			"ops": `list of steps, each {"do":"<op>",...args} (e.g. [{"do":"post","t":5,"b":"hi"},{"do":"who"}])`,
			"stop": "1 = stop at the first error instead of running every step",
		},
		Lists: boolset("ops"), Bools: boolset("stop"),
		Write: true, WantsMe: true,
		Handler: opBatch,
	})
}

func opBatch(ctx context.Context, r *Req) (any, error) {
	ops := r.List("ops")
	if len(ops) == 0 {
		return nil, badHint(`batch needs ops=[{"do":<op>,...}]`, "GET /api/skill")
	}
	if len(ops) > r.Cfg.MaxOpsPerBatch {
		return nil, badHint(fmt.Sprintf("batch has %d steps, max %d", len(ops), r.Cfg.MaxOpsPerBatch), "split it into smaller batches")
	}
	results := make([]any, 0, len(ops))
	var firstErr map[string]any
	for index, stepAny := range ops {
		step, ok := stepAny.(map[string]any)
		if !ok || step["do"] == nil || asAnyStr(step["do"]) == "" {
			return nil, badHint(`each batch step needs "do":<op name>`, "ops: "+strings.Join(sortedOpNames(), ", "))
		}
		name := asAnyStr(step["do"])
		if name == "batch" {
			return nil, bad("batch cannot nest", "put every step in one flat list")
		}
		args := map[string]any{}
		for k, v := range step {
			if k != "do" {
				args[k] = v
			}
		}
		res, err := Run(ctx, r.DB, r.Cfg, name, args, r.Me, false, "", "")
		if err != nil {
			ae, isAPI := err.(*ApiError)
			if !isAPI {
				return nil, err
			}
			entry := map[string]any{"do": name}
			for k, v := range ae.Body() {
				entry[k] = v
			}
			results = append(results, entry)
			if firstErr == nil {
				fe := map[string]any{"at": index}
				for k, v := range entry {
					fe[k] = v
				}
				firstErr = fe
			}
			if r.Bool("stop") {
				break
			}
			continue
		}
		results = append(results, map[string]any{"do": name, "r": res})
	}
	out := map[string]any{"ok": 0, "r": results, "seq": MaxSeq(ctx, r.DB)}
	if firstErr == nil {
		out["ok"] = 1
	} else {
		out["err"] = firstErr
	}
	return out, nil
}
