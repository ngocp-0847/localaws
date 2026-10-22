package main

// IAM (Query protocol): roles, managed policies (with versions), inline policies,
// attachments, users and access keys, instance profiles, OIDC providers, tags.
// Policies are stored and read back exactly — never evaluated: nothing in localaws is
// authorised against them. AWS managed policies (arn:aws:iam::aws:policy/…) exist
// implicitly, so attaching AmazonECSTaskExecutionRolePolicy & co. works.
// Documents come back URL-encoded, as IAM returns them.

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

const iamNS = `xmlns="https://iam.amazonaws.com/doc/2010-05-08/"`

type IAMRole struct {
	Name, Path, ID, AssumeDoc, Description, Boundary string
	MaxSession                                       int
	Created                                          int64
	Inline                                           map[string]string
	Attached                                         []string
}

type PolicyVersion struct {
	ID      string `json:"id"`
	Doc     string `json:"doc"`
	Created int64  `json:"created"`
}

type IAMPolicy struct {
	Arn, Name, Path, ID, Description, Default string
	Versions                                  []PolicyVersion
	Created, Updated                          int64
}

type IAMUser struct {
	Name, Path, ID, Boundary string
	Created                  int64
	Inline                   map[string]string
	Attached                 []string
	Keys                     []AccessKey
}

type AccessKey struct {
	ID, Secret, Status string
	Created            int64
}

type InstanceProfile struct {
	Name, Path, ID string
	Roles          []string
	Created        int64
}

type OIDCProvider struct {
	Arn, URL    string
	ClientIDs   []string
	Thumbprints []string
	Created     int64
}

func iamID(prefix string) string { return prefix + strings.ToUpper(randHex(9)) }

func urlDoc(doc string) string { return strings.ReplaceAll(url.QueryEscape(doc), "+", "%20") }

func normPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func (a *App) iamArn(kind, path, name string) string {
	return "arn:aws:iam::" + a.cfg.Account + ":" + kind + normPath(path) + name
}

func validDoc(doc string) *apiError {
	var v map[string]any
	if json.Unmarshal([]byte(doc), &v) != nil {
		return errf(400, "MalformedPolicyDocument", "The policy failed legacy parsing")
	}
	return nil
}

func noEntity(kind, name string) *apiError {
	return errf(404, "NoSuchEntity", "The %s with name %s cannot be found.", kind, name)
}

func membersXML(l []string) string {
	var sb strings.Builder
	for _, s := range l {
		sb.WriteString("<member>" + xe(s) + "</member>")
	}
	return sb.String()
}

func (a *App) iamTagsXML(arn string) string {
	t := a.tagsOf(arn)
	if len(t) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<Tags>")
	for _, k := range sortedKeys(t) {
		sb.WriteString("<member><Key>" + xe(k) + "</Key><Value>" + xe(t[k]) + "</Value></member>")
	}
	sb.WriteString("</Tags>")
	return sb.String()
}

func (a *App) roleArn(r IAMRole) string { return a.iamArn("role", r.Path, r.Name) }

func (a *App) roleXML(r IAMRole) string {
	s := "<Path>" + xe(r.Path) + "</Path><RoleName>" + xe(r.Name) + "</RoleName><RoleId>" + r.ID + "</RoleId><Arn>" + xe(a.roleArn(r)) +
		"</Arn><CreateDate>" + isoMs(r.Created) + "</CreateDate><AssumeRolePolicyDocument>" + urlDoc(r.AssumeDoc) +
		"</AssumeRolePolicyDocument><MaxSessionDuration>" + strconv.Itoa(r.MaxSession) + "</MaxSessionDuration><RoleLastUsed/>"
	if r.Description != "" {
		s += "<Description>" + xe(r.Description) + "</Description>"
	}
	if r.Boundary != "" {
		s += "<PermissionsBoundary><PermissionsBoundaryType>Policy</PermissionsBoundaryType><PermissionsBoundaryArn>" + xe(r.Boundary) +
			"</PermissionsBoundaryArn></PermissionsBoundary>"
	}
	return s + a.iamTagsXML(a.roleArn(r))
}

func (a *App) policyXML(p IAMPolicy) string {
	s := "<PolicyName>" + xe(p.Name) + "</PolicyName><PolicyId>" + p.ID + "</PolicyId><Arn>" + xe(p.Arn) + "</Arn><Path>" + xe(p.Path) +
		"</Path><DefaultVersionId>" + p.Default + "</DefaultVersionId><AttachmentCount>" + strconv.Itoa(a.attachments(p.Arn)) +
		"</AttachmentCount><PermissionsBoundaryUsageCount>0</PermissionsBoundaryUsageCount><IsAttachable>true</IsAttachable><CreateDate>" +
		isoMs(p.Created) + "</CreateDate><UpdateDate>" + isoMs(p.Updated) + "</UpdateDate>"
	if p.Description != "" {
		s += "<Description>" + xe(p.Description) + "</Description>"
	}
	return s + a.iamTagsXML(p.Arn)
}

func (a *App) userArn(u IAMUser) string { return a.iamArn("user", u.Path, u.Name) }

func (a *App) userXML(u IAMUser) string {
	s := "<Path>" + xe(u.Path) + "</Path><UserName>" + xe(u.Name) + "</UserName><UserId>" + u.ID + "</UserId><Arn>" + xe(a.userArn(u)) +
		"</Arn><CreateDate>" + isoMs(u.Created) + "</CreateDate>"
	if u.Boundary != "" {
		s += "<PermissionsBoundary><PermissionsBoundaryType>Policy</PermissionsBoundaryType><PermissionsBoundaryArn>" + xe(u.Boundary) +
			"</PermissionsBoundaryArn></PermissionsBoundary>"
	}
	return s + a.iamTagsXML(a.userArn(u))
}

func (a *App) profileArn(p InstanceProfile) string {
	return a.iamArn("instance-profile", p.Path, p.Name)
}

func (a *App) profileXML(p InstanceProfile) string {
	var roles strings.Builder
	for _, n := range p.Roles {
		var r IAMRole
		if a.db.get("iamrole", n, &r) {
			roles.WriteString("<member>" + a.roleXML(r) + "</member>")
		}
	}
	return "<Path>" + xe(p.Path) + "</Path><InstanceProfileName>" + xe(p.Name) + "</InstanceProfileName><InstanceProfileId>" + p.ID +
		"</InstanceProfileId><Arn>" + xe(a.profileArn(p)) + "</Arn><CreateDate>" + isoMs(p.Created) + "</CreateDate><Roles>" + roles.String() + "</Roles>" +
		a.iamTagsXML(a.profileArn(p))
}

// managedPolicy finds a customer managed policy, or makes up an AWS managed one.
func (a *App) managedPolicy(arn string) (IAMPolicy, bool) {
	var p IAMPolicy
	if a.db.get("iampolicy", arn, &p) {
		return p, true
	}
	if strings.HasPrefix(arn, "arn:aws:iam::aws:policy/") {
		path := strings.TrimPrefix(arn, "arn:aws:iam::aws:policy")
		i := strings.LastIndex(path, "/")
		return IAMPolicy{Arn: arn, Name: path[i+1:], Path: path[:i+1], ID: "ANPA" + strings.ToUpper(md5Hex([]byte(arn))[:17]), Default: "v1",
			Versions: []PolicyVersion{{ID: "v1", Doc: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`, Created: a.started}},
			Created:  a.started, Updated: a.started}, true
	}
	return p, false
}

func (a *App) attachments(arn string) int {
	n := 0
	for _, r := range list[IAMRole](a.db, "iamrole") {
		if contains(r.Attached, arn) {
			n++
		}
	}
	for _, u := range list[IAMUser](a.db, "iamuser") {
		if contains(u.Attached, arn) {
			n++
		}
	}
	return n
}

func remove(l []string, s string) []string {
	out := l[:0]
	for _, x := range l {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func (a *App) iamQuery(v url.Values) (string, *apiError) {
	action := v.Get("Action")
	meta := "<ResponseMetadata><RequestId>" + uuid4() + "</RequestId></ResponseMetadata>"
	wrap := func(inner string) string {
		return "<" + action + "Response " + iamNS + "><" + action + "Result>" + inner + "</" + action + "Result>" + meta + "</" + action + "Response>"
	}
	empty := "<" + action + "Response " + iamNS + ">" + meta + "</" + action + "Response>"
	getRole := func() (IAMRole, *apiError) {
		var r IAMRole
		if !a.db.get("iamrole", v.Get("RoleName"), &r) {
			return r, noEntity("role", v.Get("RoleName"))
		}
		return r, nil
	}
	getUser := func() (IAMUser, *apiError) {
		var u IAMUser
		if !a.db.get("iamuser", v.Get("UserName"), &u) {
			return u, noEntity("user", v.Get("UserName"))
		}
		return u, nil
	}
	tagsList := func(arn string) string {
		t := a.tagsOf(arn)
		var sb strings.Builder
		for _, k := range sortedKeys(t) {
			sb.WriteString("<member><Key>" + xe(k) + "</Key><Value>" + xe(t[k]) + "</Value></member>")
		}
		return wrap("<Tags>" + sb.String() + "</Tags><IsTruncated>false</IsTruncated>")
	}

	switch action {
	// ── roles
	case "CreateRole":
		name := v.Get("RoleName")
		if a.db.has("iamrole", name) {
			return "", errf(409, "EntityAlreadyExists", "Role with name %s already exists.", name)
		}
		doc := v.Get("AssumeRolePolicyDocument")
		if e := validDoc(doc); e != nil {
			return "", e
		}
		r := IAMRole{Name: name, Path: normPath(v.Get("Path")), ID: iamID("AROA"), AssumeDoc: doc, Description: v.Get("Description"),
			Boundary: v.Get("PermissionsBoundary"), MaxSession: 3600, Created: nowMs(), Inline: map[string]string{}}
		if n := atoi(v.Get("MaxSessionDuration")); n > 0 {
			r.MaxSession = n
		}
		_ = a.db.put("iamrole", name, r)
		a.tagResource(a.roleArn(r), queryTags(v, "Tags"))
		return wrap("<Role>" + a.roleXML(r) + "</Role>"), nil
	case "GetRole":
		r, e := getRole()
		if e != nil {
			return "", e
		}
		return wrap("<Role>" + a.roleXML(r) + "</Role>"), nil
	case "ListRoles":
		var sb strings.Builder
		for _, r := range list[IAMRole](a.db, "iamrole") {
			if p := v.Get("PathPrefix"); p == "" || strings.HasPrefix(r.Path, p) {
				sb.WriteString("<member>" + a.roleXML(r) + "</member>")
			}
		}
		return wrap("<Roles>" + sb.String() + "</Roles><IsTruncated>false</IsTruncated>"), nil
	case "UpdateRole", "UpdateRoleDescription":
		r, e := getRole()
		if e != nil {
			return "", e
		}
		if _, ok := v["Description"]; ok {
			r.Description = v.Get("Description")
		}
		if n := atoi(v.Get("MaxSessionDuration")); n > 0 {
			r.MaxSession = n
		}
		_ = a.db.put("iamrole", r.Name, r)
		if action == "UpdateRoleDescription" {
			return wrap("<Role>" + a.roleXML(r) + "</Role>"), nil
		}
		return wrap(""), nil
	case "UpdateAssumeRolePolicy":
		r, e := getRole()
		if e != nil {
			return "", e
		}
		if e := validDoc(v.Get("PolicyDocument")); e != nil {
			return "", e
		}
		r.AssumeDoc = v.Get("PolicyDocument")
		_ = a.db.put("iamrole", r.Name, r)
		return empty, nil
	case "PutRolePermissionsBoundary", "DeleteRolePermissionsBoundary":
		r, e := getRole()
		if e != nil {
			return "", e
		}
		r.Boundary = v.Get("PermissionsBoundary")
		_ = a.db.put("iamrole", r.Name, r)
		return empty, nil
	case "DeleteRole":
		r, e := getRole()
		if e != nil {
			return "", e
		}
		if len(r.Inline) > 0 || len(r.Attached) > 0 {
			return "", errf(409, "DeleteConflict", "Cannot delete entity, must detach all policies first.")
		}
		for _, p := range list[InstanceProfile](a.db, "iamprofile") {
			if contains(p.Roles, r.Name) {
				return "", errf(409, "DeleteConflict", "Cannot delete entity, must remove roles from instance profile first.")
			}
		}
		_ = a.db.del("iamrole", r.Name)
		a.untagAll(a.roleArn(r))
		return empty, nil
	case "CreateServiceLinkedRole":
		svc := v.Get("AWSServiceName")
		base := strings.Split(svc, ".")[0]
		name := "AWSServiceRoleFor" + strings.ToUpper(base[:1]) + base[1:] + v.Get("CustomSuffix")
		var r IAMRole
		if !a.db.get("iamrole", name, &r) {
			r = IAMRole{Name: name, Path: "/aws-service-role/" + svc + "/", ID: iamID("AROA"), Description: v.Get("Description"), MaxSession: 3600,
				Created: nowMs(), Inline: map[string]string{},
				AssumeDoc: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"` + svc + `"},"Action":"sts:AssumeRole"}]}`}
			_ = a.db.put("iamrole", name, r)
		}
		return wrap("<Role>" + a.roleXML(r) + "</Role>"), nil
	case "DeleteServiceLinkedRole":
		r, e := getRole()
		if e != nil {
			return "", e
		}
		_ = a.db.del("iamrole", r.Name)
		return wrap("<DeletionTaskId>task/" + r.Path + r.Name + "/" + uuid4() + "</DeletionTaskId>"), nil
	case "GetServiceLinkedRoleDeletionStatus":
		return wrap("<Status>SUCCEEDED</Status>"), nil
	case "TagRole", "UntagRole", "ListRoleTags":
		r, e := getRole()
		if e != nil {
			return "", e
		}
		switch action {
		case "TagRole":
			a.tagResource(a.roleArn(r), queryTags(v, "Tags"))
		case "UntagRole":
			a.untagResource(a.roleArn(r), members(v, "TagKeys"))
		default:
			return tagsList(a.roleArn(r)), nil
		}
		return empty, nil

	// ── inline policies (roles and users)
	case "PutRolePolicy", "PutUserPolicy":
		if e := validDoc(v.Get("PolicyDocument")); e != nil {
			return "", e
		}
		if action == "PutRolePolicy" {
			r, e := getRole()
			if e != nil {
				return "", e
			}
			if r.Inline == nil {
				r.Inline = map[string]string{}
			}
			r.Inline[v.Get("PolicyName")] = v.Get("PolicyDocument")
			_ = a.db.put("iamrole", r.Name, r)
		} else {
			u, e := getUser()
			if e != nil {
				return "", e
			}
			if u.Inline == nil {
				u.Inline = map[string]string{}
			}
			u.Inline[v.Get("PolicyName")] = v.Get("PolicyDocument")
			_ = a.db.put("iamuser", u.Name, u)
		}
		return empty, nil
	case "GetRolePolicy", "GetUserPolicy", "DeleteRolePolicy", "DeleteUserPolicy", "ListRolePolicies", "ListUserPolicies":
		var inline map[string]string
		owner, ownerKey := v.Get("RoleName"), "RoleName"
		var r IAMRole
		var u IAMUser
		isRole := strings.Contains(action, "Role")
		if isRole {
			var e *apiError
			if r, e = getRole(); e != nil {
				return "", e
			}
			inline = r.Inline
		} else {
			var e *apiError
			if u, e = getUser(); e != nil {
				return "", e
			}
			inline, owner, ownerKey = u.Inline, u.Name, "UserName"
		}
		name := v.Get("PolicyName")
		switch {
		case strings.HasPrefix(action, "List"):
			return wrap("<PolicyNames>" + membersXML(sortedKeys(inline)) + "</PolicyNames><IsTruncated>false</IsTruncated>"), nil
		case strings.HasPrefix(action, "Get"):
			doc, ok := inline[name]
			if !ok {
				return "", errf(404, "NoSuchEntity", "The %s policy with name %s cannot be found.", strings.ToLower(ownerKey[:4]), name)
			}
			return wrap("<" + ownerKey + ">" + xe(owner) + "</" + ownerKey + "><PolicyName>" + xe(name) + "</PolicyName><PolicyDocument>" + urlDoc(doc) + "</PolicyDocument>"), nil
		}
		if _, ok := inline[name]; !ok {
			return "", errf(404, "NoSuchEntity", "The %s policy with name %s cannot be found.", strings.ToLower(ownerKey[:4]), name)
		}
		delete(inline, name)
		if isRole {
			_ = a.db.put("iamrole", r.Name, r)
		} else {
			_ = a.db.put("iamuser", u.Name, u)
		}
		return empty, nil

	// ── attachments
	case "AttachRolePolicy", "DetachRolePolicy", "AttachUserPolicy", "DetachUserPolicy":
		arn := v.Get("PolicyArn")
		if _, ok := a.managedPolicy(arn); !ok {
			return "", errf(404, "NoSuchEntity", "Policy %s does not exist or is not attachable.", arn)
		}
		attach := strings.HasPrefix(action, "Attach")
		if strings.Contains(action, "Role") {
			r, e := getRole()
			if e != nil {
				return "", e
			}
			if attach && !contains(r.Attached, arn) {
				r.Attached = append(r.Attached, arn)
			} else if !attach {
				if !contains(r.Attached, arn) {
					return "", errf(404, "NoSuchEntity", "Policy %s was not found.", arn)
				}
				r.Attached = remove(r.Attached, arn)
			}
			_ = a.db.put("iamrole", r.Name, r)
		} else {
			u, e := getUser()
			if e != nil {
				return "", e
			}
			if attach && !contains(u.Attached, arn) {
				u.Attached = append(u.Attached, arn)
			} else if !attach {
				if !contains(u.Attached, arn) {
					return "", errf(404, "NoSuchEntity", "Policy %s was not found.", arn)
				}
				u.Attached = remove(u.Attached, arn)
			}
			_ = a.db.put("iamuser", u.Name, u)
		}
		return empty, nil
	case "ListAttachedRolePolicies", "ListAttachedUserPolicies":
		var attached []string
		if action == "ListAttachedRolePolicies" {
			r, e := getRole()
			if e != nil {
				return "", e
			}
			attached = r.Attached
		} else {
			u, e := getUser()
			if e != nil {
				return "", e
			}
			attached = u.Attached
		}
		var sb strings.Builder
		for _, arn := range attached {
			sb.WriteString("<member><PolicyName>" + xe(arnTail(arn)) + "</PolicyName><PolicyArn>" + xe(arn) + "</PolicyArn></member>")
		}
		return wrap("<AttachedPolicies>" + sb.String() + "</AttachedPolicies><IsTruncated>false</IsTruncated>"), nil
	case "ListEntitiesForPolicy":
		arn := v.Get("PolicyArn")
		var roles, users strings.Builder
		for _, r := range list[IAMRole](a.db, "iamrole") {
			if contains(r.Attached, arn) {
				roles.WriteString("<member><RoleName>" + xe(r.Name) + "</RoleName><RoleId>" + r.ID + "</RoleId></member>")
			}
		}
		for _, u := range list[IAMUser](a.db, "iamuser") {
			if contains(u.Attached, arn) {
				users.WriteString("<member><UserName>" + xe(u.Name) + "</UserName><UserId>" + u.ID + "</UserId></member>")
			}
		}
		return wrap("<PolicyGroups/><PolicyUsers>" + users.String() + "</PolicyUsers><PolicyRoles>" + roles.String() + "</PolicyRoles><IsTruncated>false</IsTruncated>"), nil

	// ── managed policies
	case "CreatePolicy":
		doc := v.Get("PolicyDocument")
		if e := validDoc(doc); e != nil {
			return "", e
		}
		p := IAMPolicy{Name: v.Get("PolicyName"), Path: normPath(v.Get("Path")), ID: iamID("ANPA"), Description: v.Get("Description"), Default: "v1",
			Versions: []PolicyVersion{{ID: "v1", Doc: doc, Created: nowMs()}}, Created: nowMs(), Updated: nowMs()}
		p.Arn = a.iamArn("policy", p.Path, p.Name)
		if a.db.has("iampolicy", p.Arn) {
			return "", errf(409, "EntityAlreadyExists", "A policy called %s already exists. Duplicate names are not allowed.", p.Name)
		}
		_ = a.db.put("iampolicy", p.Arn, p)
		a.tagResource(p.Arn, queryTags(v, "Tags"))
		return wrap("<Policy>" + a.policyXML(p) + "</Policy>"), nil
	case "GetPolicy":
		p, ok := a.managedPolicy(v.Get("PolicyArn"))
		if !ok {
			return "", errf(404, "NoSuchEntity", "Policy %s was not found.", v.Get("PolicyArn"))
		}
		return wrap("<Policy>" + a.policyXML(p) + "</Policy>"), nil
	case "ListPolicies":
		var sb strings.Builder
		for _, p := range list[IAMPolicy](a.db, "iampolicy") {
			sb.WriteString("<member>" + a.policyXML(p) + "</member>")
		}
		return wrap("<Policies>" + sb.String() + "</Policies><IsTruncated>false</IsTruncated>"), nil
	case "GetPolicyVersion", "ListPolicyVersions", "CreatePolicyVersion", "DeletePolicyVersion", "SetDefaultPolicyVersion", "DeletePolicy":
		p, ok := a.managedPolicy(v.Get("PolicyArn"))
		if !ok {
			return "", errf(404, "NoSuchEntity", "Policy %s was not found.", v.Get("PolicyArn"))
		}
		verXML := func(pv PolicyVersion) string {
			return "<Document>" + urlDoc(pv.Doc) + "</Document><VersionId>" + pv.ID + "</VersionId><IsDefaultVersion>" + boolS(pv.ID == p.Default) +
				"</IsDefaultVersion><CreateDate>" + isoMs(pv.Created) + "</CreateDate>"
		}
		switch action {
		case "GetPolicyVersion":
			for _, pv := range p.Versions {
				if pv.ID == v.Get("VersionId") {
					return wrap("<PolicyVersion>" + verXML(pv) + "</PolicyVersion>"), nil
				}
			}
			return "", errf(404, "NoSuchEntity", "Policy %s version %s does not exist or is not attachable.", p.Arn, v.Get("VersionId"))
		case "ListPolicyVersions":
			var sb strings.Builder
			for i := len(p.Versions) - 1; i >= 0; i-- {
				sb.WriteString("<member>" + verXML(p.Versions[i]) + "</member>")
			}
			return wrap("<Versions>" + sb.String() + "</Versions><IsTruncated>false</IsTruncated>"), nil
		case "CreatePolicyVersion":
			if len(p.Versions) >= 5 {
				return "", errf(409, "LimitExceeded", "A managed policy can have up to 5 versions.")
			}
			if e := validDoc(v.Get("PolicyDocument")); e != nil {
				return "", e
			}
			n := 0
			for _, pv := range p.Versions {
				if k := atoi(strings.TrimPrefix(pv.ID, "v")); k > n {
					n = k
				}
			}
			pv := PolicyVersion{ID: "v" + strconv.Itoa(n+1), Doc: v.Get("PolicyDocument"), Created: nowMs()}
			p.Versions = append(p.Versions, pv)
			if v.Get("SetAsDefault") == "true" {
				p.Default = pv.ID
			}
			p.Updated = nowMs()
			_ = a.db.put("iampolicy", p.Arn, p)
			return wrap("<PolicyVersion>" + verXML(pv) + "</PolicyVersion>"), nil
		case "DeletePolicyVersion":
			if v.Get("VersionId") == p.Default {
				return "", errf(409, "DeleteConflict", "Cannot delete the default version of a policy.")
			}
			kept := p.Versions[:0]
			for _, pv := range p.Versions {
				if pv.ID != v.Get("VersionId") {
					kept = append(kept, pv)
				}
			}
			p.Versions = kept
			_ = a.db.put("iampolicy", p.Arn, p)
		case "SetDefaultPolicyVersion":
			p.Default, p.Updated = v.Get("VersionId"), nowMs()
			_ = a.db.put("iampolicy", p.Arn, p)
		case "DeletePolicy":
			if a.attachments(p.Arn) > 0 {
				return "", errf(409, "DeleteConflict", "Cannot delete a policy attached to entities.")
			}
			_ = a.db.del("iampolicy", p.Arn)
			a.untagAll(p.Arn)
		}
		return empty, nil
	case "TagPolicy", "UntagPolicy", "ListPolicyTags":
		arn := v.Get("PolicyArn")
		if _, ok := a.managedPolicy(arn); !ok {
			return "", errf(404, "NoSuchEntity", "Policy %s was not found.", arn)
		}
		switch action {
		case "TagPolicy":
			a.tagResource(arn, queryTags(v, "Tags"))
		case "UntagPolicy":
			a.untagResource(arn, members(v, "TagKeys"))
		default:
			return tagsList(arn), nil
		}
		return empty, nil

	// ── users
	case "CreateUser":
		name := v.Get("UserName")
		if a.db.has("iamuser", name) {
			return "", errf(409, "EntityAlreadyExists", "User with name %s already exists.", name)
		}
		u := IAMUser{Name: name, Path: normPath(v.Get("Path")), ID: iamID("AIDA"), Boundary: v.Get("PermissionsBoundary"), Created: nowMs(), Inline: map[string]string{}}
		_ = a.db.put("iamuser", name, u)
		a.tagResource(a.userArn(u), queryTags(v, "Tags"))
		return wrap("<User>" + a.userXML(u) + "</User>"), nil
	case "GetUser":
		if v.Get("UserName") == "" { // the caller
			arn := a.cfg.IdentityArn()
			return wrap("<User><Path>/</Path><UserName>" + xe(arnTail(arn)) + "</UserName><UserId>AIDALOCALAWS</UserId><Arn>" + xe(arn) +
				"</Arn><CreateDate>" + isoMs(a.started) + "</CreateDate></User>"), nil
		}
		u, e := getUser()
		if e != nil {
			return "", e
		}
		return wrap("<User>" + a.userXML(u) + "</User>"), nil
	case "UpdateUser":
		u, e := getUser()
		if e != nil {
			return "", e
		}
		_ = a.db.del("iamuser", u.Name)
		if n := v.Get("NewUserName"); n != "" {
			u.Name = n
		}
		if p := v.Get("NewPath"); p != "" {
			u.Path = p
		}
		_ = a.db.put("iamuser", u.Name, u)
		return empty, nil
	case "ListUsers":
		var sb strings.Builder
		for _, u := range list[IAMUser](a.db, "iamuser") {
			sb.WriteString("<member>" + a.userXML(u) + "</member>")
		}
		return wrap("<Users>" + sb.String() + "</Users><IsTruncated>false</IsTruncated>"), nil
	case "DeleteUser":
		u, e := getUser()
		if e != nil {
			return "", e
		}
		if len(u.Inline) > 0 || len(u.Attached) > 0 || len(u.Keys) > 0 {
			return "", errf(409, "DeleteConflict", "Cannot delete entity, must delete policies and access keys first.")
		}
		_ = a.db.del("iamuser", u.Name)
		a.untagAll(a.userArn(u))
		return empty, nil
	case "PutUserPermissionsBoundary", "DeleteUserPermissionsBoundary":
		u, e := getUser()
		if e != nil {
			return "", e
		}
		u.Boundary = v.Get("PermissionsBoundary")
		_ = a.db.put("iamuser", u.Name, u)
		return empty, nil
	case "TagUser", "UntagUser", "ListUserTags":
		u, e := getUser()
		if e != nil {
			return "", e
		}
		switch action {
		case "TagUser":
			a.tagResource(a.userArn(u), queryTags(v, "Tags"))
		case "UntagUser":
			a.untagResource(a.userArn(u), members(v, "TagKeys"))
		default:
			return tagsList(a.userArn(u)), nil
		}
		return empty, nil
	case "CreateAccessKey", "ListAccessKeys", "DeleteAccessKey", "UpdateAccessKey":
		u, e := getUser()
		if e != nil {
			return "", e
		}
		switch action {
		case "CreateAccessKey":
			k := AccessKey{ID: "AKIA" + strings.ToUpper(randHex(8)), Secret: randHex(20), Status: "Active", Created: nowMs()}
			u.Keys = append(u.Keys, k)
			_ = a.db.put("iamuser", u.Name, u)
			return wrap("<AccessKey><UserName>" + xe(u.Name) + "</UserName><AccessKeyId>" + k.ID + "</AccessKeyId><Status>Active</Status><SecretAccessKey>" +
				k.Secret + "</SecretAccessKey><CreateDate>" + isoMs(k.Created) + "</CreateDate></AccessKey>"), nil
		case "ListAccessKeys":
			var sb strings.Builder
			for _, k := range u.Keys {
				sb.WriteString("<member><UserName>" + xe(u.Name) + "</UserName><AccessKeyId>" + k.ID + "</AccessKeyId><Status>" + k.Status +
					"</Status><CreateDate>" + isoMs(k.Created) + "</CreateDate></member>")
			}
			return wrap("<AccessKeyMetadata>" + sb.String() + "</AccessKeyMetadata><IsTruncated>false</IsTruncated>"), nil
		case "UpdateAccessKey":
			for i := range u.Keys {
				if u.Keys[i].ID == v.Get("AccessKeyId") {
					u.Keys[i].Status = v.Get("Status")
				}
			}
		default:
			kept := u.Keys[:0]
			for _, k := range u.Keys {
				if k.ID != v.Get("AccessKeyId") {
					kept = append(kept, k)
				}
			}
			u.Keys = kept
		}
		_ = a.db.put("iamuser", u.Name, u)
		return empty, nil
	case "GetLoginProfile":
		return "", noEntity("login profile", v.Get("UserName"))
	case "ListGroupsForUser":
		return wrap("<Groups/><IsTruncated>false</IsTruncated>"), nil
	case "ListMFADevices":
		return wrap("<MFADevices/><IsTruncated>false</IsTruncated>"), nil
	case "ListSSHPublicKeys":
		return wrap("<SSHPublicKeys/><IsTruncated>false</IsTruncated>"), nil
	case "ListServiceSpecificCredentials":
		return wrap("<ServiceSpecificCredentials/>"), nil
	case "ListSigningCertificates":
		return wrap("<Certificates/><IsTruncated>false</IsTruncated>"), nil
	case "ListAccountAliases":
		return wrap("<AccountAliases/><IsTruncated>false</IsTruncated>"), nil

	// ── instance profiles
	case "CreateInstanceProfile":
		name := v.Get("InstanceProfileName")
		if a.db.has("iamprofile", name) {
			return "", errf(409, "EntityAlreadyExists", "Instance Profile %s already exists.", name)
		}
		p := InstanceProfile{Name: name, Path: normPath(v.Get("Path")), ID: iamID("AIPA"), Created: nowMs()}
		_ = a.db.put("iamprofile", name, p)
		a.tagResource(a.profileArn(p), queryTags(v, "Tags"))
		return wrap("<InstanceProfile>" + a.profileXML(p) + "</InstanceProfile>"), nil
	case "GetInstanceProfile", "DeleteInstanceProfile", "AddRoleToInstanceProfile", "RemoveRoleFromInstanceProfile",
		"TagInstanceProfile", "UntagInstanceProfile", "ListInstanceProfileTags":
		var p InstanceProfile
		if !a.db.get("iamprofile", v.Get("InstanceProfileName"), &p) {
			return "", noEntity("instance profile", v.Get("InstanceProfileName"))
		}
		switch action {
		case "GetInstanceProfile":
			return wrap("<InstanceProfile>" + a.profileXML(p) + "</InstanceProfile>"), nil
		case "DeleteInstanceProfile":
			if len(p.Roles) > 0 {
				return "", errf(409, "DeleteConflict", "Cannot delete entity, must remove roles from instance profile first.")
			}
			_ = a.db.del("iamprofile", p.Name)
			a.untagAll(a.profileArn(p))
			return empty, nil
		case "AddRoleToInstanceProfile":
			if _, e := getRole(); e != nil {
				return "", e
			}
			if len(p.Roles) > 0 {
				return "", errf(409, "LimitExceeded", "Cannot exceed quota for InstanceSessionsPerInstanceProfile: 1")
			}
			p.Roles = append(p.Roles, v.Get("RoleName"))
		case "RemoveRoleFromInstanceProfile":
			p.Roles = remove(p.Roles, v.Get("RoleName"))
		case "TagInstanceProfile":
			a.tagResource(a.profileArn(p), queryTags(v, "Tags"))
		case "UntagInstanceProfile":
			a.untagResource(a.profileArn(p), members(v, "TagKeys"))
		default:
			return tagsList(a.profileArn(p)), nil
		}
		_ = a.db.put("iamprofile", p.Name, p)
		return empty, nil
	case "ListInstanceProfiles", "ListInstanceProfilesForRole":
		var sb strings.Builder
		for _, p := range list[InstanceProfile](a.db, "iamprofile") {
			if action == "ListInstanceProfilesForRole" && !contains(p.Roles, v.Get("RoleName")) {
				continue
			}
			sb.WriteString("<member>" + a.profileXML(p) + "</member>")
		}
		return wrap("<InstanceProfiles>" + sb.String() + "</InstanceProfiles><IsTruncated>false</IsTruncated>"), nil

	// ── OIDC providers
	case "CreateOpenIDConnectProvider":
		u := strings.TrimPrefix(v.Get("Url"), "https://")
		p := OIDCProvider{Arn: "arn:aws:iam::" + a.cfg.Account + ":oidc-provider/" + u, URL: u, ClientIDs: members(v, "ClientIDList"),
			Thumbprints: members(v, "ThumbprintList"), Created: nowMs()}
		if a.db.has("oidc", p.Arn) {
			return "", errf(409, "EntityAlreadyExists", "Provider with url %s already exists.", v.Get("Url"))
		}
		_ = a.db.put("oidc", p.Arn, p)
		a.tagResource(p.Arn, queryTags(v, "Tags"))
		return wrap("<OpenIDConnectProviderArn>" + xe(p.Arn) + "</OpenIDConnectProviderArn>" + a.iamTagsXML(p.Arn)), nil
	case "ListOpenIDConnectProviders":
		var sb strings.Builder
		for _, p := range list[OIDCProvider](a.db, "oidc") {
			sb.WriteString("<member><Arn>" + xe(p.Arn) + "</Arn></member>")
		}
		return wrap("<OpenIDConnectProviderList>" + sb.String() + "</OpenIDConnectProviderList>"), nil
	case "GetOpenIDConnectProvider", "DeleteOpenIDConnectProvider", "UpdateOpenIDConnectProviderThumbprint", "AddClientIDToOpenIDConnectProvider",
		"RemoveClientIDFromOpenIDConnectProvider", "TagOpenIDConnectProvider", "UntagOpenIDConnectProvider", "ListOpenIDConnectProviderTags":
		var p OIDCProvider
		arn := v.Get("OpenIDConnectProviderArn")
		if !a.db.get("oidc", arn, &p) {
			return "", errf(404, "NoSuchEntity", "OpenIDConnect Provider not found for arn %s", arn)
		}
		switch action {
		case "GetOpenIDConnectProvider":
			return wrap("<Url>" + xe(p.URL) + "</Url><ClientIDList>" + membersXML(p.ClientIDs) + "</ClientIDList><ThumbprintList>" +
				membersXML(p.Thumbprints) + "</ThumbprintList><CreateDate>" + isoMs(p.Created) + "</CreateDate>" + a.iamTagsXML(arn)), nil
		case "DeleteOpenIDConnectProvider":
			_ = a.db.del("oidc", arn)
			a.untagAll(arn)
			return empty, nil
		case "UpdateOpenIDConnectProviderThumbprint":
			p.Thumbprints = members(v, "ThumbprintList")
		case "AddClientIDToOpenIDConnectProvider":
			if !contains(p.ClientIDs, v.Get("ClientID")) {
				p.ClientIDs = append(p.ClientIDs, v.Get("ClientID"))
			}
		case "RemoveClientIDFromOpenIDConnectProvider":
			p.ClientIDs = remove(p.ClientIDs, v.Get("ClientID"))
		case "TagOpenIDConnectProvider":
			a.tagResource(arn, queryTags(v, "Tags"))
		case "UntagOpenIDConnectProvider":
			a.untagResource(arn, members(v, "TagKeys"))
		default:
			return tagsList(arn), nil
		}
		_ = a.db.put("oidc", arn, p)
		return empty, nil
	}
	return "", errf(400, "InvalidAction", "localaws iam: action %s is not implemented", action)
}
