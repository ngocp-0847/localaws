package main

// Tags, for every service, kept by resource ARN. Terraform (and anything else that
// manages resources declaratively) reads tags back after writing them and plans a change
// when they differ, so they are stored rather than accepted and dropped. Deleting a
// resource drops its tags (untagAll).

import "sort"

func (a *App) tagsOf(arn string) map[string]string {
	t := map[string]string{}
	a.db.get("tags", arn, &t)
	return t
}

func (a *App) tagResource(arn string, add map[string]string) {
	if len(add) == 0 {
		return
	}
	t := a.tagsOf(arn)
	for k, v := range add {
		t[k] = v
	}
	_ = a.db.put("tags", arn, t)
}

func (a *App) untagResource(arn string, keys []string) {
	t := a.tagsOf(arn)
	for _, k := range keys {
		delete(t, k)
	}
	_ = a.db.put("tags", arn, t)
}

func (a *App) untagAll(arn string) { _ = a.db.del("tags", arn) }

// tagList renders tags as [{<k>: key, <v>: value}] — Key/Value for most JSON APIs,
// key/value for ECS, TagKey/TagValue for KMS.
func tagList(t map[string]string, k, v string) []any {
	out := []any{}
	for _, key := range sortedKeys(t) {
		out = append(out, M{k: key, v: t[key]})
	}
	return out
}

// tagsFromList reads [{<k>: key, <v>: value}] into a map.
func tagsFromList(l []any, k, v string) map[string]string {
	out := map[string]string{}
	for _, x := range l {
		m, _ := x.(map[string]any)
		if m != nil && getStr(m, k) != "" {
			out[getStr(m, k)] = getStr(m, v)
		}
	}
	return out
}

func sortedStrings(l []string) []string {
	out := append([]string(nil), l...)
	sort.Strings(out)
	return out
}
