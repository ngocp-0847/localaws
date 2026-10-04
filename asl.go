package main

// Amazon States Language: paths, Parameters / ResultSelector templates, intrinsic
// functions and Choice rules — the data-flow half of the interpreter (sfn_run.go is
// the control-flow half). Pure functions, unit-tested in asl_test.go.

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ── JSONPath (reference paths: $.a.b, $['a'], $.a[0], $$.Execution.Id) ──────
func jsonPath(path string, data any) (any, error) {
	path = strings.TrimSpace(path)
	if path == "$" {
		return data, nil
	}
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("invalid path %q", path)
	}
	cur := data
	rest := path[1:]
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("the JSONPath '%s' could not be found in the input", path)
			}
			v, ok := m[rest[:end]]
			if !ok {
				return nil, fmt.Errorf("the JSONPath '%s' could not be found in the input", path)
			}
			cur, rest = v, rest[end:]
		case strings.HasPrefix(rest, "["):
			end := strings.Index(rest, "]")
			if end < 0 {
				return nil, fmt.Errorf("invalid path %q", path)
			}
			idx := rest[1:end]
			rest = rest[end+1:]
			if strings.HasPrefix(idx, "'") || strings.HasPrefix(idx, `"`) {
				m, ok := cur.(map[string]any)
				v, has := m[strings.Trim(idx, `'"`)]
				if !ok || !has {
					return nil, fmt.Errorf("the JSONPath '%s' could not be found in the input", path)
				}
				cur = v
				continue
			}
			n, err := strconv.Atoi(idx)
			l, ok := cur.([]any)
			if err != nil || !ok {
				return nil, fmt.Errorf("the JSONPath '%s' could not be found in the input", path)
			}
			if n < 0 {
				n += len(l)
			}
			if n < 0 || n >= len(l) {
				return nil, fmt.Errorf("the JSONPath '%s' could not be found in the input", path)
			}
			cur = l[n]
		default:
			return nil, fmt.Errorf("invalid path %q", path)
		}
	}
	return cur, nil
}

// pathIn resolves "$…" against the input and "$$…" against the context object.
func pathIn(p string, input, ctx any) (any, error) {
	if strings.HasPrefix(p, "$$") {
		return jsonPath(p[1:], ctx)
	}
	return jsonPath(p, input)
}

// setPath implements ResultPath: "$" replaces, "$.a.b" merges into a copy of the input.
func setPath(input any, path string, value any) (any, error) {
	if path == "$" {
		return value, nil
	}
	if !strings.HasPrefix(path, "$.") {
		return nil, fmt.Errorf("invalid ResultPath %q", path)
	}
	root, ok := deepCopy(input).(map[string]any)
	if !ok {
		if input != nil {
			return nil, fmt.Errorf("unable to apply ResultPath %q to input of type %T", path, input)
		}
		root = map[string]any{}
	}
	keys := strings.Split(path[2:], ".")
	cur := root
	for i, k := range keys {
		if i == len(keys)-1 {
			cur[k] = value
			break
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	return root, nil
}

func deepCopy(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// ── payload templates (Parameters, ResultSelector, ItemSelector) ───────────
func resolveTemplate(t any, input, ctx any) (any, error) {
	switch v := t.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range v {
			if strings.HasSuffix(k, ".$") {
				s, ok := val.(string)
				if !ok {
					return nil, fmt.Errorf("the value for the field '%s' must be a STRING that contains a JSONPath or an intrinsic", k)
				}
				r, err := evalExpr(s, input, ctx)
				if err != nil {
					return nil, fmt.Errorf("the value for the field '%s' %v", k, err)
				}
				out[strings.TrimSuffix(k, ".$")] = r
				continue
			}
			r, err := resolveTemplate(val, input, ctx)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			r, err := resolveTemplate(x, input, ctx)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return t, nil
}

// ── intrinsic functions ──────────────────────────────────────────────────
func evalExpr(expr string, input, ctx any) (any, error) {
	expr = strings.TrimSpace(expr)
	switch {
	case strings.HasPrefix(expr, "States."):
		return evalIntrinsic(expr, input, ctx)
	case strings.HasPrefix(expr, "$"):
		return pathIn(expr, input, ctx)
	case strings.HasPrefix(expr, "'"):
		if len(expr) < 2 || !strings.HasSuffix(expr, "'") {
			return nil, fmt.Errorf("unterminated string %s", expr)
		}
		return unescapeLiteral(expr[1 : len(expr)-1]), nil
	}
	var v any
	if err := json.Unmarshal([]byte(expr), &v); err != nil {
		return nil, fmt.Errorf("cannot evaluate %q", expr)
	}
	return v, nil
}

func unescapeLiteral(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func splitArgs(s string) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted && c == '\\':
			i++
		case quoted:
			if c == '\'' {
				quoted = false
			}
		case c == '\'':
			quoted = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		out = append(out, t)
	}
	return out
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func evalIntrinsic(expr string, input, ctx any) (any, error) {
	open := strings.Index(expr, "(")
	if open < 0 || !strings.HasSuffix(expr, ")") {
		return nil, fmt.Errorf("invalid intrinsic %q", expr)
	}
	name := expr[:open]
	var args []any
	for _, raw := range splitArgs(expr[open+1 : len(expr)-1]) {
		v, err := evalExpr(raw, input, ctx)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	need := func(n int) error {
		if len(args) < n {
			return fmt.Errorf("%s expects at least %d argument(s)", name, n)
		}
		return nil
	}
	arr := func(i int) ([]any, error) {
		l, ok := args[i].([]any)
		if !ok {
			return nil, fmt.Errorf("%s: argument %d must be an array", name, i+1)
		}
		return l, nil
	}
	switch name {
	case "States.Array":
		if args == nil {
			return []any{}, nil
		}
		return args, nil
	case "States.Format":
		if err := need(1); err != nil {
			return nil, err
		}
		tpl, _ := args[0].(string)
		var b strings.Builder
		ai := 1
		for i := 0; i < len(tpl); i++ {
			switch {
			case tpl[i] == '\\' && i+1 < len(tpl):
				i++
				b.WriteByte(tpl[i])
			case tpl[i] == '{' && i+1 < len(tpl) && tpl[i+1] == '}':
				if ai >= len(args) {
					return nil, fmt.Errorf("States.Format: not enough arguments for the template")
				}
				b.WriteString(str(args[ai]))
				ai++
				i++
			default:
				b.WriteByte(tpl[i])
			}
		}
		return b.String(), nil
	case "States.StringToJson":
		if err := need(1); err != nil {
			return nil, err
		}
		var v any
		err := json.Unmarshal([]byte(str(args[0])), &v)
		return v, err
	case "States.JsonToString":
		if err := need(1); err != nil {
			return nil, err
		}
		return jsonStr(args[0]), nil
	case "States.ArrayPartition":
		if err := need(2); err != nil {
			return nil, err
		}
		l, err := arr(0)
		if err != nil {
			return nil, err
		}
		size, _ := num(args[1])
		if size < 1 {
			return nil, fmt.Errorf("States.ArrayPartition: chunk size must be > 0")
		}
		out := []any{}
		for i := 0; i < len(l); i += int(size) {
			end := i + int(size)
			if end > len(l) {
				end = len(l)
			}
			out = append(out, append([]any{}, l[i:end]...))
		}
		return out, nil
	case "States.ArrayContains":
		if err := need(2); err != nil {
			return nil, err
		}
		l, err := arr(0)
		if err != nil {
			return nil, err
		}
		for _, x := range l {
			if jsonStr(x) == jsonStr(args[1]) {
				return true, nil
			}
		}
		return false, nil
	case "States.ArrayRange":
		if err := need(3); err != nil {
			return nil, err
		}
		a, _ := num(args[0])
		b, _ := num(args[1])
		s, _ := num(args[2])
		if s == 0 {
			return nil, fmt.Errorf("States.ArrayRange: step must not be 0")
		}
		out := []any{}
		for x := a; (s > 0 && x <= b) || (s < 0 && x >= b); x += s {
			out = append(out, x)
			if len(out) > 1000 {
				return nil, fmt.Errorf("States.ArrayRange: more than 1000 items")
			}
		}
		return out, nil
	case "States.ArrayGetItem":
		if err := need(2); err != nil {
			return nil, err
		}
		l, err := arr(0)
		if err != nil {
			return nil, err
		}
		i, _ := num(args[1])
		if int(i) < 0 || int(i) >= len(l) {
			return nil, fmt.Errorf("States.ArrayGetItem: index %d out of range", int(i))
		}
		return l[int(i)], nil
	case "States.ArrayLength":
		if err := need(1); err != nil {
			return nil, err
		}
		l, err := arr(0)
		if err != nil {
			return nil, err
		}
		return float64(len(l)), nil
	case "States.ArrayUnique":
		if err := need(1); err != nil {
			return nil, err
		}
		l, err := arr(0)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		out := []any{}
		for _, x := range l {
			if k := jsonStr(x); !seen[k] {
				seen[k] = true
				out = append(out, x)
			}
		}
		return out, nil
	case "States.Base64Encode":
		if err := need(1); err != nil {
			return nil, err
		}
		return base64.StdEncoding.EncodeToString([]byte(str(args[0]))), nil
	case "States.Base64Decode":
		if err := need(1); err != nil {
			return nil, err
		}
		b, err := base64.StdEncoding.DecodeString(str(args[0]))
		return string(b), err
	case "States.Hash":
		if err := need(2); err != nil {
			return nil, err
		}
		var h hash.Hash
		switch str(args[1]) {
		case "MD5":
			h = md5.New()
		case "SHA-1":
			h = sha1.New()
		case "SHA-256":
			h = sha256.New()
		case "SHA-384":
			h = sha512.New384()
		case "SHA-512":
			h = sha512.New()
		default:
			return nil, fmt.Errorf("States.Hash: unsupported algorithm %q", str(args[1]))
		}
		h.Write([]byte(str(args[0])))
		return hex.EncodeToString(h.Sum(nil)), nil
	case "States.JsonMerge":
		if err := need(2); err != nil {
			return nil, err
		}
		x, ok1 := args[0].(map[string]any)
		y, ok2 := args[1].(map[string]any)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("States.JsonMerge: both arguments must be objects")
		}
		out := map[string]any{}
		for k, v := range x {
			out[k] = v
		}
		for k, v := range y {
			out[k] = v
		}
		return out, nil
	case "States.MathRandom":
		if err := need(2); err != nil {
			return nil, err
		}
		lo, _ := num(args[0])
		hi, _ := num(args[1])
		if hi <= lo {
			return nil, fmt.Errorf("States.MathRandom: end must be greater than start")
		}
		return float64(int(lo) + rand.Intn(int(hi)-int(lo))), nil
	case "States.MathAdd":
		if err := need(2); err != nil {
			return nil, err
		}
		x, _ := num(args[0])
		y, _ := num(args[1])
		return x + y, nil
	case "States.StringSplit":
		if err := need(2); err != nil {
			return nil, err
		}
		seps := str(args[1])
		parts := strings.FieldsFunc(str(args[0]), func(r rune) bool { return strings.ContainsRune(seps, r) })
		out := []any{}
		for _, p := range parts {
			out = append(out, p)
		}
		return out, nil
	case "States.UUID":
		return uuid4(), nil
	}
	return nil, fmt.Errorf("the intrinsic function %s is not supported", name)
}

// ── Choice rules ─────────────────────────────────────────────────────────
var tsLayouts = []string{time.RFC3339Nano, time.RFC3339}

func parseTS(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	for _, l := range tsLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

var choiceOps = regexp.MustCompile(`^(String|Numeric|Boolean|Timestamp)(Equals|LessThan|GreaterThan|LessThanEquals|GreaterThanEquals|Matches)(Path)?$`)

// evalChoice evaluates one rule (possibly And/Or/Not) against the state input.
func evalChoice(rule map[string]any, input, ctx any) (bool, error) {
	if l, ok := rule["And"].([]any); ok {
		for _, x := range l {
			m, _ := x.(map[string]any)
			ok, err := evalChoice(m, input, ctx)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	}
	if l, ok := rule["Or"].([]any); ok {
		for _, x := range l {
			m, _ := x.(map[string]any)
			ok, err := evalChoice(m, input, ctx)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	if n, ok := rule["Not"].(map[string]any); ok {
		ok, err := evalChoice(n, input, ctx)
		return !ok && err == nil, err
	}
	variable := str(rule["Variable"])
	val, verr := pathIn(variable, input, ctx)
	present := verr == nil
	for k, operand := range rule {
		switch k {
		case "Variable", "Next", "Comment":
			continue
		case "IsPresent":
			return present == operand.(bool), nil
		}
		if !present {
			return false, fmt.Errorf("invalid path '%s': the choice state's condition path references an invalid value", variable)
		}
		switch k {
		case "IsNull":
			return (val == nil) == operand.(bool), nil
		case "IsString":
			_, ok := val.(string)
			return ok == operand.(bool), nil
		case "IsNumeric":
			_, ok := val.(float64)
			return ok == operand.(bool), nil
		case "IsBoolean":
			_, ok := val.(bool)
			return ok == operand.(bool), nil
		case "IsTimestamp":
			_, ok := parseTS(val)
			return ok == operand.(bool), nil
		}
		m := choiceOps.FindStringSubmatch(k)
		if m == nil {
			return false, fmt.Errorf("unsupported choice operator %s", k)
		}
		kind, cmp, isPath := m[1], m[2], m[3] != ""
		if isPath {
			v, err := pathIn(str(operand), input, ctx)
			if err != nil {
				return false, err
			}
			operand = v
		}
		return compare(kind, cmp, val, operand), nil
	}
	return false, fmt.Errorf("choice rule has no comparison")
}

func compare(kind, cmp string, a, b any) bool {
	var c int
	switch kind {
	case "String":
		x, ok1 := a.(string)
		y, ok2 := b.(string)
		if !ok1 || !ok2 {
			return false
		}
		if cmp == "Matches" {
			return wildcard(strings.ReplaceAll(y, `\*`, "\x00"), x) // escaped '*' is literal
		}
		c = strings.Compare(x, y)
	case "Numeric":
		x, ok1 := a.(float64)
		y, ok2 := b.(float64)
		if !ok1 || !ok2 {
			return false
		}
		c = map[bool]int{true: -1, false: 0}[x < y]
		if x > y {
			c = 1
		}
		if math.IsNaN(x) || math.IsNaN(y) {
			return false
		}
	case "Boolean":
		x, ok1 := a.(bool)
		y, ok2 := b.(bool)
		return ok1 && ok2 && cmp == "Equals" && x == y
	case "Timestamp":
		x, ok1 := parseTS(a)
		y, ok2 := parseTS(b)
		if !ok1 || !ok2 {
			return false
		}
		c = x.Compare(y)
	}
	switch cmp {
	case "Equals":
		return c == 0
	case "LessThan":
		return c < 0
	case "GreaterThan":
		return c > 0
	case "LessThanEquals":
		return c <= 0
	case "GreaterThanEquals":
		return c >= 0
	}
	return false
}

// ── definition validation (CreateStateMachine) ───────────────────────────
func validateDefinition(def map[string]any, where string) []string {
	var errs []string
	states, ok := def["States"].(map[string]any)
	if !ok || len(states) == 0 {
		return []string{fmt.Sprintf("SCHEMA_VALIDATION_FAILED: The field 'States' should be non-empty at %s", where)}
	}
	start := str(def["StartAt"])
	if _, ok := states[start]; !ok {
		errs = append(errs, fmt.Sprintf("MISSING_TRANSITION_TARGET: Missing 'Next' target: %s at %s/StartAt", start, where))
	}
	known := map[string]bool{"Task": true, "Pass": true, "Choice": true, "Wait": true, "Succeed": true, "Fail": true, "Parallel": true, "Map": true}
	for name, raw := range states {
		st, _ := raw.(map[string]any)
		at := where + "/States/" + name
		typ := str(st["Type"])
		if !known[typ] {
			errs = append(errs, fmt.Sprintf("SCHEMA_VALIDATION_FAILED: unknown state type %q at %s", typ, at))
			continue
		}
		targets := []string{}
		if n := str(st["Next"]); n != "" {
			targets = append(targets, n)
		}
		if n := str(st["Default"]); n != "" {
			targets = append(targets, n)
		}
		for _, c := range append(getList(st, "Choices"), getList(st, "Catch")...) {
			if cm, ok := c.(map[string]any); ok {
				targets = append(targets, str(cm["Next"]))
			}
		}
		for _, t := range targets {
			if _, ok := states[t]; !ok {
				errs = append(errs, fmt.Sprintf("MISSING_TRANSITION_TARGET: Missing 'Next' target: %s at %s", t, at))
			}
		}
		end, _ := st["End"].(bool)
		if typ != "Choice" && typ != "Succeed" && typ != "Fail" && !end && str(st["Next"]) == "" {
			errs = append(errs, fmt.Sprintf("SCHEMA_VALIDATION_FAILED: state %s has neither Next nor End at %s", name, at))
		}
		if typ == "Task" && str(st["Resource"]) == "" {
			errs = append(errs, fmt.Sprintf("SCHEMA_VALIDATION_FAILED: The field 'Resource' should be non-null at %s", at))
		}
		for i, b := range getList(st, "Branches") {
			bm, _ := b.(map[string]any)
			errs = append(errs, validateDefinition(bm, fmt.Sprintf("%s/Branches[%d]", at, i))...)
		}
		if typ == "Map" {
			proc := getMap(st, "ItemProcessor")
			if len(proc) == 0 {
				proc = getMap(st, "Iterator")
			}
			errs = append(errs, validateDefinition(proc, at+"/ItemProcessor")...)
		}
	}
	return errs
}
