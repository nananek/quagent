package toolserver

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	methods    = []string{"get", "post", "put", "patch", "delete"}
	nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

// convert は OpenAPI 仕様 (3.x) の operation をツールに変換する。ツール名は
// "<サーバー名>__<operationId>" (operationId が無ければメソッドとパスから作る)。
func convert(server string, spec map[string]any) (map[string]*operation, error) {
	paths, _ := spec["paths"].(map[string]any)
	if len(paths) == 0 {
		return nil, fmt.Errorf("OpenAPI 仕様に paths が無い (OpenAPI 3.x の JSON か確認する)")
	}
	pathKeys := make([]string, 0, len(paths))
	for p := range paths {
		pathKeys = append(pathKeys, p)
	}
	sort.Strings(pathKeys)

	ops := map[string]*operation{}
	for _, p := range pathKeys {
		item, _ := resolveRef(spec, paths[p]).(map[string]any)
		if item == nil {
			continue
		}
		shared, _ := item["parameters"].([]any)
		for _, m := range methods {
			o, _ := item[m].(map[string]any)
			if o == nil {
				continue
			}
			op := buildOperation(spec, server, p, m, o, shared)
			op.toolName = uniqueName(ops, op.toolName)
			ops[op.toolName] = op
		}
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("OpenAPI 仕様にツールにできる operation が無い")
	}
	return ops, nil
}

func buildOperation(spec map[string]any, server, path, method string, o map[string]any, shared []any) *operation {
	op := &operation{method: strings.ToUpper(method), path: path, bodyKey: "body"}

	id, _ := o["operationId"].(string)
	if id == "" {
		id = method + "_" + path
	}
	op.toolName = toolName(server, id)

	desc := strings.TrimSpace(str(o["summary"]))
	if d := strings.TrimSpace(str(o["description"])); d != "" && d != desc {
		if desc != "" {
			desc += "\n\n"
		}
		desc += d
	}
	if desc == "" {
		desc = op.method + " " + path
	}
	op.description = desc

	props := map[string]any{}
	var required []string

	// path レベルの parameters は operation 側の同名のもので上書きされる
	merged := map[string]map[string]any{}
	var order []string
	for _, list := range [][]any{shared, anySlice(o["parameters"])} {
		for _, raw := range list {
			pm, _ := resolveRef(spec, raw).(map[string]any)
			name, in := str(pm["name"]), str(pm["in"])
			if name == "" || (in != "path" && in != "query" && in != "header") {
				continue // cookie などは扱わない
			}
			key := in + ":" + name
			if _, seen := merged[key]; !seen {
				order = append(order, key)
			}
			merged[key] = pm
		}
	}
	for _, key := range order {
		pm := merged[key]
		name, in := str(pm["name"]), str(pm["in"])
		req, _ := pm["required"].(bool)
		if in == "path" {
			req = true
		}
		explode := true
		if e, ok := pm["explode"].(bool); ok {
			explode = e
		}
		op.params = append(op.params, param{name: name, in: in, required: req, explode: explode})

		var schema map[string]any
		if sc, ok := resolve(spec, pm["schema"], nil, 0).(map[string]any); ok {
			schema = sc
		} else {
			schema = map[string]any{}
		}
		if d := str(pm["description"]); d != "" && str(schema["description"]) == "" {
			schema["description"] = d
		}
		props[name] = schema
		if req {
			required = append(required, name)
		}
	}

	// 本文 (JSON を優先。無ければフォーム)
	if rb, _ := resolveRef(spec, o["requestBody"]).(map[string]any); rb != nil {
		content, _ := rb["content"].(map[string]any)
		ct, media := pickContent(content)
		if media != nil {
			op.hasBody = true
			op.bodyJSON = ct != "application/x-www-form-urlencoded"
			op.bodyReq, _ = rb["required"].(bool)
			if _, taken := props[op.bodyKey]; taken {
				op.bodyKey = "request_body"
			}
			schema, _ := resolve(spec, media["schema"], nil, 0).(map[string]any)
			if schema == nil {
				schema = map[string]any{}
			}
			if d := str(rb["description"]); d != "" && str(schema["description"]) == "" {
				schema["description"] = d
			}
			props[op.bodyKey] = schema
			if op.bodyReq {
				required = append(required, op.bodyKey)
			}
		}
	}

	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	op.schema = schema
	return op
}

// pickContent は requestBody の content から使う形式を選ぶ。
func pickContent(content map[string]any) (string, map[string]any) {
	if m, ok := content["application/json"].(map[string]any); ok {
		return "application/json", m
	}
	keys := make([]string, 0, len(content))
	for k := range content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasSuffix(strings.SplitN(k, ";", 2)[0], "json") {
			m, _ := content[k].(map[string]any)
			return "application/json", m
		}
	}
	if m, ok := content["application/x-www-form-urlencoded"].(map[string]any); ok {
		return "application/x-www-form-urlencoded", m
	}
	return "", nil
}

// toolName は "<server>__<operationId>" を MCP のツール名にする (使える文字だけ、長さ制限内)。
func toolName(server, id string) string {
	id = strings.Trim(nameUnsafe.ReplaceAllString(id, "_"), "_")
	if id == "" {
		id = "tool"
	}
	n := server + NameSep + id
	if len(n) > maxToolName {
		n = n[:maxToolName]
	}
	return n
}

func uniqueName(ops map[string]*operation, name string) string {
	if _, taken := ops[name]; !taken {
		return name
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf("_%d", i)
		base := name
		if len(base)+len(suffix) > maxToolName {
			base = base[:maxToolName-len(suffix)]
		}
		if _, taken := ops[base+suffix]; !taken {
			return base + suffix
		}
	}
}

// resolveRef は {"$ref": "#/..."} なら参照先を返す (そうでなければ v のまま)。
func resolveRef(spec map[string]any, v any) any {
	for range maxSchemaDepth {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		ref, ok := m["$ref"].(string)
		if !ok {
			return v
		}
		target, ok := lookup(spec, ref)
		if !ok {
			return map[string]any{}
		}
		v = target
	}
	return map[string]any{}
}

// lookup は "#/components/schemas/X" のような JSON Pointer を引く。
func lookup(spec map[string]any, ref string) (any, bool) {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil, false // 外部ファイルへの参照は扱わない
	}
	var cur any = spec
	for _, part := range strings.Split(rest, "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// resolve は schema の $ref を展開して返す (元の仕様は書き換えない)。循環参照は
// 空のオブジェクトにして打ち切る。
func resolve(spec map[string]any, v any, stack []string, depth int) any {
	if depth > maxSchemaDepth {
		return map[string]any{}
	}
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok {
			for _, s := range stack {
				if s == ref {
					return map[string]any{"type": "object"}
				}
			}
			target, ok := lookup(spec, ref)
			if !ok {
				return map[string]any{}
			}
			return resolve(spec, target, append(stack[:len(stack):len(stack)], ref), depth+1)
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = resolve(spec, e, stack, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = resolve(spec, e, stack, depth+1)
		}
		return out
	default:
		return v
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}
