package main

import (
	"encoding/json"
	"testing"
)

func j(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic(err)
	}
	return v
}

func TestJSONPath(t *testing.T) {
	in := j(`{"a":{"b":[10,{"c":"x"}]},"k y":1}`)
	for path, want := range map[string]string{"$": jsonStr(in), "$.a.b[0]": "10", "$.a.b[1].c": `"x"`, "$['k y']": "1", "$.a.b[-1].c": `"x"`} {
		got, err := jsonPath(path, in)
		if err != nil || jsonStr(got) != want {
			t.Errorf("%s = %v (%v), want %s", path, jsonStr(got), err, want)
		}
	}
	if _, err := jsonPath("$.missing", in); err == nil {
		t.Error("missing path must fail")
	}
}

func TestIntrinsics(t *testing.T) {
	in := j(`{"key":"daily/file.csv","n":3,"arr":[1,2,2,3],"o1":{"a":1},"o2":{"b":2}}`)
	ctx := j(`{"Execution":{"Name":"run-1"}}`)
	cases := map[string]string{
		`States.Format('--s3-key={}', $.key)`:                        `"--s3-key=daily/file.csv"`,
		`States.Array('node', 'main.js', $.n)`:                       `["node","main.js",3]`,
		`States.Format('{} \{literal\} {}', $$.Execution.Name, 'x')`: `"run-1 {literal} x"`,
		`States.ArrayLength($.arr)`:                                  `4`,
		`States.ArrayUnique($.arr)`:                                  `[1,2,3]`,
		`States.ArrayContains($.arr, 3)`:                             `true`,
		`States.ArrayPartition($.arr, 3)`:                            `[[1,2,2],[3]]`,
		`States.ArrayRange(1, 9, 4)`:                                 `[1,5,9]`,
		`States.ArrayGetItem($.arr, 3)`:                              `3`,
		`States.JsonMerge($.o1, $.o2, false)`:                        `{"a":1,"b":2}`,
		`States.StringToJson('{"x":1}')`:                             `{"x":1}`,
		`States.JsonToString($.o1)`:                                  `"{\"a\":1}"`,
		`States.Base64Encode('hi')`:                                  `"aGk="`,
		`States.Base64Decode('aGk=')`:                                `"hi"`,
		`States.Hash('abc', 'SHA-256')`:                              `"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"`,
		`States.MathAdd($.n, -1)`:                                    `2`,
		`States.StringSplit('a,b;c', ',;')`:                          `["a","b","c"]`,
	}
	for expr, want := range cases {
		got, err := evalExpr(expr, in, ctx)
		if err != nil || jsonStr(got) != want {
			t.Errorf("%s = %s (%v), want %s", expr, jsonStr(got), err, want)
		}
	}
}

func TestTemplatesAndResultPath(t *testing.T) {
	tpl := j(`{"Cluster":"c","Overrides":{"ContainerOverrides":[{"Name":"w","Command.$":"States.Array('run', States.Format('--key={}', $.detail.object.key))"}]},"Exec.$":"$$.Execution.Name"}`)
	got, err := resolveTemplate(tpl, j(`{"detail":{"object":{"key":"k1"}}}`), j(`{"Execution":{"Name":"e1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Cluster":"c","Exec":"e1","Overrides":{"ContainerOverrides":[{"Command":["run","--key=k1"],"Name":"w"}]}}`
	if jsonStr(got) != want {
		t.Errorf("template = %s\nwant      %s", jsonStr(got), want)
	}
	out, _ := setPath(j(`{"a":1}`), "$.r.x", "v")
	if jsonStr(out) != `{"a":1,"r":{"x":"v"}}` {
		t.Errorf("ResultPath = %s", jsonStr(out))
	}
}

func TestChoice(t *testing.T) {
	in := j(`{"n":5,"s":"report-2031.csv","b":true,"ts":"2031-01-02T03:04:05Z","limit":4}`)
	cases := map[string]bool{
		`{"Variable":"$.n","NumericGreaterThan":4}`:                                                   true,
		`{"Variable":"$.n","NumericLessThanEqualsPath":"$.limit"}`:                                    false,
		`{"Variable":"$.s","StringMatches":"report-*.csv"}`:                                           true,
		`{"Variable":"$.b","BooleanEquals":false}`:                                                    false,
		`{"Variable":"$.ts","TimestampGreaterThan":"2030-12-31T00:00:00Z"}`:                           true,
		`{"Variable":"$.missing","IsPresent":false}`:                                                  true,
		`{"And":[{"Variable":"$.n","IsNumeric":true},{"Not":{"Variable":"$.s","StringEquals":"x"}}]}`: true,
		`{"Or":[{"Variable":"$.n","NumericEquals":1},{"Variable":"$.n","NumericEquals":2}]}`:          false,
	}
	for rule, want := range cases {
		got, err := evalChoice(j(rule).(map[string]any), in, M{})
		if err != nil || got != want {
			t.Errorf("%s = %v (%v), want %v", rule, got, err, want)
		}
	}
}

func TestValidateDefinition(t *testing.T) {
	ok := j(`{"StartAt":"A","States":{"A":{"Type":"Pass","Next":"B"},"B":{"Type":"Succeed"}}}`).(map[string]any)
	if errs := validateDefinition(ok, "/"); len(errs) != 0 {
		t.Errorf("valid definition rejected: %v", errs)
	}
	bad := j(`{"StartAt":"A","States":{"A":{"Type":"Pass","Next":"Nope"}}}`).(map[string]any)
	if errs := validateDefinition(bad, "/"); len(errs) == 0 {
		t.Error("missing transition target accepted")
	}
}

func TestEventPatterns(t *testing.T) {
	ev := j(`{"source":"aws.s3","detail-type":"Object Created","detail":{"bucket":{"name":"in"},"object":{"key":"daily/a.csv","size":120},"tags":["x","y"],"ip":"10.0.1.5"}}`).(map[string]any)
	cases := map[string]bool{
		`{"source":["aws.s3"],"detail":{"object":{"key":[{"prefix":"daily/"}]}}}`:                 true,
		`{"detail":{"object":{"key":[{"wildcard":"daily/*.csv"}]}}}`:                              true,
		`{"detail":{"object":{"key":[{"suffix":".zip"}]}}}`:                                       false,
		`{"detail":{"object":{"size":[{"numeric":[">",100,"<=",200]}]}}}`:                         true,
		`{"detail":{"tags":["y"]}}`:                                                               true,
		`{"detail":{"missing":[{"exists":false}]}}`:                                               true,
		`{"detail":{"bucket":{"name":[{"anything-but":["in","out"]}]}}}`:                          false,
		`{"detail":{"ip":[{"cidr":"10.0.0.0/16"}]}}`:                                              true,
		`{"$or":[{"source":["nope"]},{"detail-type":[{"equals-ignore-case":"object created"}]}]}`: true,
	}
	for p, want := range cases {
		if got := matchPattern(j(p).(map[string]any), ev); got != want {
			t.Errorf("%s = %v, want %v", p, got, want)
		}
	}
}

func TestFilterPattern(t *testing.T) {
	line := `{"level":"ERROR","code":"E42","n":7}`
	for p, want := range map[string]bool{`ERROR E42`: true, `"ERROR" -E42`: false, `?WARN ?ERROR`: true,
		`{ $.level = "ERROR" && $.n > 5 }`: true, `{ $.code = "E4*" }`: true, `{ $.n < 5 }`: false} {
		if got := filterMatch(p, line); got != want {
			t.Errorf("%s = %v, want %v", p, got, want)
		}
	}
}
