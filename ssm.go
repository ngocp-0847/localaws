package main

// SSM Parameter Store: Put (versions, Overwrite), Get / GetParameters /
// GetParametersByPath / DescribeParameters / Delete(s) / GetParameterHistory.
// SecureString values come back encrypted-looking unless WithDecryption is set — code
// that forgets the flag fails here the way it fails on AWS. ECS `secrets` resolve here.

import (
	"encoding/base64"
	"sort"
	"strings"
)

type Param struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Value       string `json:"value"`
	Version     int    `json:"version"`
	Modified    int64  `json:"modified"`
	Description string `json:"description,omitempty"`
}

func paramName(ref string) string {
	if strings.HasPrefix(ref, "arn:") {
		if i := strings.Index(ref, ":parameter"); i >= 0 {
			ref = ref[i+len(":parameter"):]
		}
	}
	if i := strings.LastIndex(ref, ":"); i > 0 {
		ref = ref[:i] // name:version / name:label → name (current version only)
	}
	return ref
}

func (a *App) param(name string) (Param, bool) {
	var p Param
	ok := a.db.get("param", paramName(name), &p)
	return p, ok
}

func (a *App) putParam(p Param, overwrite bool) (Param, *apiError) {
	old, exists := a.param(p.Name)
	if exists && !overwrite {
		return old, errf(400, "ParameterAlreadyExists", "The parameter already exists. To overwrite this value, set the overwrite option in the request to true.")
	}
	if p.Type == "" {
		if !exists {
			return p, errf(400, "ValidationException", "A parameter type is required when you create a parameter.")
		}
		p.Type = old.Type
	}
	if p.Type != "String" && p.Type != "StringList" && p.Type != "SecureString" {
		return p, errf(400, "ValidationException", "1 validation error detected: Value '%s' at 'type' failed to satisfy constraint", p.Type)
	}
	p.Version = old.Version + 1
	p.Modified = nowMs()
	return p, errOrNil(a.db.put("param", p.Name, p))
}

func errOrNil(err error) *apiError {
	if err != nil {
		return errf(500, "InternalServerError", "%v", err)
	}
	return nil
}

func (a *App) paramOut(p Param, decrypt bool) M {
	v := p.Value
	if p.Type == "SecureString" && !decrypt {
		v = "AQICAH" + base64.StdEncoding.EncodeToString([]byte("localaws:"+p.Value))
	}
	return M{"Name": p.Name, "Type": p.Type, "Value": v, "Version": p.Version, "LastModifiedDate": epoch(p.Modified),
		"ARN": a.arn("ssm", "parameter"+ensureSlash(p.Name)), "DataType": "text"}
}

func ensureSlash(n string) string {
	if strings.HasPrefix(n, "/") {
		return n
	}
	return "/" + n
}

func (a *App) ssmAPI(op string, in M) (any, *apiError) {
	all := func() []Param {
		ps := list[Param](a.db, "param")
		sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
		return ps
	}
	notFound := errf(400, "ParameterNotFound", "")
	switch op {
	case "PutParameter":
		p, e := a.putParam(Param{Name: getStr(in, "Name"), Type: getStr(in, "Type"), Value: getStr(in, "Value"),
			Description: getStr(in, "Description")}, getBool(in, "Overwrite"))
		if e != nil {
			return nil, e
		}
		return M{"Version": p.Version, "Tier": "Standard"}, nil
	case "GetParameter":
		p, ok := a.param(getStr(in, "Name"))
		if !ok {
			return nil, notFound
		}
		return M{"Parameter": a.paramOut(p, getBool(in, "WithDecryption"))}, nil
	case "GetParameters":
		out, bad := []any{}, []any{}
		for _, n := range strList(getList(in, "Names")) {
			if p, ok := a.param(n); ok {
				out = append(out, a.paramOut(p, getBool(in, "WithDecryption")))
			} else {
				bad = append(bad, n)
			}
		}
		return M{"Parameters": out, "InvalidParameters": bad}, nil
	case "GetParametersByPath":
		path := getStr(in, "Path")
		if !strings.HasSuffix(path, "/") {
			path += "/"
		}
		var out []any
		for _, p := range all() {
			rest := strings.TrimPrefix(p.Name, path)
			if strings.HasPrefix(p.Name, path) && (getBool(in, "Recursive") || !strings.Contains(rest, "/")) {
				out = append(out, a.paramOut(p, getBool(in, "WithDecryption")))
			}
		}
		page, next := pageOf(out, getStr(in, "NextToken"), getInt(in, "MaxResults", 10))
		resp := M{"Parameters": page}
		if next != "" {
			resp["NextToken"] = next
		}
		return resp, nil
	case "DescribeParameters":
		var out []any
		for _, p := range all() {
			if paramFiltered(p, in) {
				m := a.paramOut(p, false)
				delete(m, "Value")
				m["Tier"], m["Policies"] = "Standard", []any{}
				if p.Description != "" {
					m["Description"] = p.Description
				}
				out = append(out, m)
			}
		}
		page, next := pageOf(out, getStr(in, "NextToken"), getInt(in, "MaxResults", 50))
		resp := M{"Parameters": page}
		if next != "" {
			resp["NextToken"] = next
		}
		return resp, nil
	case "GetParameterHistory":
		p, ok := a.param(getStr(in, "Name"))
		if !ok {
			return nil, notFound
		}
		return M{"Parameters": []any{a.paramOut(p, getBool(in, "WithDecryption"))}}, nil
	case "DeleteParameter":
		if _, ok := a.param(getStr(in, "Name")); !ok {
			return nil, notFound
		}
		_ = a.db.del("param", paramName(getStr(in, "Name")))
		return M{}, nil
	case "DeleteParameters":
		done, bad := []any{}, []any{}
		for _, n := range strList(getList(in, "Names")) {
			if _, ok := a.param(n); ok {
				_ = a.db.del("param", paramName(n))
				done = append(done, n)
			} else {
				bad = append(bad, n)
			}
		}
		return M{"DeletedParameters": done, "InvalidParameters": bad}, nil
	case "AddTagsToResource", "RemoveTagsFromResource":
		return M{}, nil
	case "ListTagsForResource":
		return M{"TagList": []any{}}, nil
	}
	return nil, errf(400, "UnknownOperationException", "localaws ssm: %s is not implemented", op)
}

func paramFiltered(p Param, in M) bool {
	for _, f := range getList(in, "ParameterFilters") {
		fm, _ := f.(map[string]any)
		vals := strList(getList(fm, "Values"))
		opt := getStr(fm, "Option")
		hit := false
		for _, v := range vals {
			switch getStr(fm, "Key") {
			case "Name":
				switch opt {
				case "BeginsWith":
					hit = hit || strings.HasPrefix(p.Name, v)
				case "Contains":
					hit = hit || strings.Contains(p.Name, v)
				default:
					hit = hit || p.Name == v
				}
			case "Path":
				pre := strings.TrimSuffix(v, "/") + "/"
				rest := strings.TrimPrefix(p.Name, pre)
				hit = hit || (strings.HasPrefix(p.Name, pre) && (opt == "Recursive" || !strings.Contains(rest, "/")))
			case "Type":
				hit = hit || p.Type == v
			default:
				hit = true
			}
		}
		if !hit {
			return false
		}
	}
	for _, f := range getList(in, "Filters") {
		fm, _ := f.(map[string]any)
		hit := false
		for _, v := range strList(getList(fm, "Values")) {
			switch getStr(fm, "Key") {
			case "Name":
				hit = hit || strings.HasPrefix(p.Name, v)
			case "Type":
				hit = hit || p.Type == v
			default:
				hit = true
			}
		}
		if !hit {
			return false
		}
	}
	return true
}
