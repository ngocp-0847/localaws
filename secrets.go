package main

// Secrets Manager (JSON, secretsmanager.*): secrets with versions and staging labels
// (AWSCURRENT / AWSPREVIOUS), descriptions, KMS key ids, resource policies, tags,
// scheduled and forced deletion, restore, GetRandomPassword. ECS `secrets` resolve here
// (whole secret, or one JSON key: <arn>:<json-key>:<stage>:<version>).

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	"strings"
	"time"
)

type SecretVersion struct {
	ID      string   `json:"id"`
	String  string   `json:"string,omitempty"`
	Binary  string   `json:"binary,omitempty"`
	Stages  []string `json:"stages"`
	Created int64    `json:"created"`
}

type Secret struct {
	Name, Arn, Description, KmsKeyID, Policy string
	Versions                                 []SecretVersion
	Created, Changed, Deleted                int64
}

func (s *Secret) staged(stage string) *SecretVersion {
	for i := range s.Versions {
		if contains(s.Versions[i].Stages, stage) {
			return &s.Versions[i]
		}
	}
	return nil
}

// secret resolves a name, a full ARN or a partial ARN (without the random suffix).
func (a *App) secret(ref string) (Secret, bool) {
	var s Secret
	if a.db.get("secret", ref, &s) {
		return s, true
	}
	for _, x := range list[Secret](a.db, "secret") {
		if x.Arn == ref || strings.HasPrefix(ref, "arn:") && strings.HasPrefix(x.Arn, ref+"-") {
			return x, true
		}
	}
	return s, false
}

func secretNotFound() *apiError {
	return errf(400, "ResourceNotFoundException", "Secrets Manager can't find the specified secret.")
}

// putVersion adds a version as AWSCURRENT; the current one becomes AWSPREVIOUS.
func (s *Secret) putVersion(id, str, bin string, stages []string) SecretVersion {
	if id == "" {
		id = uuid4()
	}
	if len(stages) == 0 {
		stages = []string{"AWSCURRENT"}
	}
	for i := range s.Versions {
		if s.Versions[i].ID == id {
			return s.Versions[i] // idempotent retry with the same ClientRequestToken
		}
	}
	if contains(stages, "AWSCURRENT") {
		for i := range s.Versions {
			s.Versions[i].Stages = remove(s.Versions[i].Stages, "AWSPREVIOUS")
		}
		for i := range s.Versions {
			if contains(s.Versions[i].Stages, "AWSCURRENT") {
				s.Versions[i].Stages = append(remove(s.Versions[i].Stages, "AWSCURRENT"), "AWSPREVIOUS")
			}
		}
	}
	v := SecretVersion{ID: id, String: str, Binary: bin, Stages: stages, Created: nowMs()}
	s.Versions = append(s.Versions, v)
	kept := s.Versions[:0]
	for _, x := range s.Versions {
		if len(x.Stages) > 0 {
			kept = append(kept, x)
		}
	}
	s.Versions = kept
	s.Changed = nowMs()
	return v
}

func (a *App) describeSecret(s Secret) M {
	stages := M{}
	for _, v := range s.Versions {
		stages[v.ID] = v.Stages
	}
	m := M{"ARN": s.Arn, "Name": s.Name, "RotationEnabled": false, "CreatedDate": epoch(s.Created), "LastChangedDate": epoch(s.Changed),
		"VersionIdsToStages": stages, "Tags": tagList(a.tagsOf(s.Arn), "Key", "Value")}
	if s.Description != "" {
		m["Description"] = s.Description
	}
	if s.KmsKeyID != "" {
		m["KmsKeyId"] = s.KmsKeyID
	}
	if s.Deleted > 0 {
		m["DeletedDate"] = epoch(s.Deleted)
	}
	return m
}

func (a *App) secretsAPI(op string, in M) (any, *apiError) {
	get := func() (Secret, *apiError) {
		s, ok := a.secret(getStr(in, "SecretId"))
		if !ok {
			return s, secretNotFound()
		}
		return s, nil
	}
	live := func() (Secret, *apiError) {
		s, e := get()
		if e == nil && s.Deleted > 0 {
			return s, errf(400, "InvalidRequestException", "You can't perform this operation on the secret because it was marked for deletion.")
		}
		return s, e
	}
	switch op {
	case "CreateSecret":
		name := getStr(in, "Name")
		if s, ok := a.secret(name); ok {
			if s.Deleted > 0 {
				return nil, errf(400, "InvalidRequestException", "You can't create this secret because a secret with this name is already scheduled for deletion.")
			}
			return nil, errf(400, "ResourceExistsException", "The operation failed because the secret %s already exists.", name)
		}
		s := Secret{Name: name, Arn: a.arn("secretsmanager", "secret:"+name+"-"+randAlnum(6)), Description: getStr(in, "Description"),
			KmsKeyID: getStr(in, "KmsKeyId"), Created: nowMs(), Changed: nowMs()}
		out := M{"ARN": s.Arn, "Name": s.Name}
		if getStr(in, "SecretString") != "" || getStr(in, "SecretBinary") != "" {
			v := s.putVersion(getStr(in, "ClientRequestToken"), getStr(in, "SecretString"), getStr(in, "SecretBinary"), nil)
			out["VersionId"] = v.ID
		}
		_ = a.db.put("secret", s.Name, s)
		a.tagResource(s.Arn, tagsFromList(getList(in, "Tags"), "Key", "Value"))
		return out, nil
	case "DescribeSecret":
		s, e := get()
		if e != nil {
			return nil, e
		}
		return a.describeSecret(s), nil
	case "GetSecretValue":
		s, e := live()
		if e != nil {
			return nil, e
		}
		var v *SecretVersion
		if id := getStr(in, "VersionId"); id != "" {
			for i := range s.Versions {
				if s.Versions[i].ID == id {
					v = &s.Versions[i]
				}
			}
		} else {
			stage := getStr(in, "VersionStage")
			if stage == "" {
				stage = "AWSCURRENT"
			}
			v = s.staged(stage)
		}
		if v == nil {
			return nil, errf(400, "ResourceNotFoundException", "Secrets Manager can't find the specified secret value for staging label: AWSCURRENT")
		}
		out := M{"ARN": s.Arn, "Name": s.Name, "VersionId": v.ID, "VersionStages": v.Stages, "CreatedDate": epoch(v.Created)}
		if v.Binary != "" {
			out["SecretBinary"] = v.Binary
		} else {
			out["SecretString"] = v.String
		}
		return out, nil
	case "BatchGetSecretValue":
		vals, errs := []any{}, []any{}
		for _, id := range strList(getList(in, "SecretIdList")) {
			out, e := a.secretsAPI("GetSecretValue", M{"SecretId": id})
			if e != nil {
				errs = append(errs, M{"SecretId": id, "ErrorCode": e.Type, "Message": e.Msg})
				continue
			}
			vals = append(vals, out)
		}
		return M{"SecretValues": vals, "Errors": errs}, nil
	case "PutSecretValue":
		s, e := live()
		if e != nil {
			return nil, e
		}
		v := s.putVersion(getStr(in, "ClientRequestToken"), getStr(in, "SecretString"), getStr(in, "SecretBinary"), strList(getList(in, "VersionStages")))
		_ = a.db.put("secret", s.Name, s)
		return M{"ARN": s.Arn, "Name": s.Name, "VersionId": v.ID, "VersionStages": v.Stages}, nil
	case "UpdateSecret":
		s, e := live()
		if e != nil {
			return nil, e
		}
		if _, ok := in["Description"]; ok {
			s.Description = getStr(in, "Description")
		}
		if _, ok := in["KmsKeyId"]; ok {
			s.KmsKeyID = getStr(in, "KmsKeyId")
		}
		out := M{"ARN": s.Arn, "Name": s.Name}
		if getStr(in, "SecretString") != "" || getStr(in, "SecretBinary") != "" {
			v := s.putVersion(getStr(in, "ClientRequestToken"), getStr(in, "SecretString"), getStr(in, "SecretBinary"), nil)
			out["VersionId"] = v.ID
		}
		s.Changed = nowMs()
		_ = a.db.put("secret", s.Name, s)
		return out, nil
	case "UpdateSecretVersionStage":
		s, e := live()
		if e != nil {
			return nil, e
		}
		stage, from, to := getStr(in, "VersionStage"), getStr(in, "RemoveFromVersionId"), getStr(in, "MoveToVersionId")
		for i := range s.Versions {
			if s.Versions[i].ID == from {
				s.Versions[i].Stages = remove(s.Versions[i].Stages, stage)
				if stage == "AWSCURRENT" {
					s.Versions[i].Stages = append(s.Versions[i].Stages, "AWSPREVIOUS")
				}
			}
			if s.Versions[i].ID == to && !contains(s.Versions[i].Stages, stage) {
				s.Versions[i].Stages = append(s.Versions[i].Stages, stage)
			}
		}
		_ = a.db.put("secret", s.Name, s)
		return M{"ARN": s.Arn, "Name": s.Name}, nil
	case "ListSecretVersionIds":
		s, e := get()
		if e != nil {
			return nil, e
		}
		out := []any{}
		for _, v := range s.Versions {
			out = append(out, M{"VersionId": v.ID, "VersionStages": v.Stages, "CreatedDate": epoch(v.Created)})
		}
		return M{"ARN": s.Arn, "Name": s.Name, "Versions": out}, nil
	case "ListSecrets":
		out := []any{}
		for _, s := range list[Secret](a.db, "secret") {
			if s.Deleted == 0 || getBool(in, "IncludePlannedDeletion") {
				out = append(out, a.describeSecret(s))
			}
		}
		return M{"SecretList": out}, nil
	case "DeleteSecret":
		s, e := get()
		if e != nil {
			return nil, e
		}
		if getBool(in, "ForceDeleteWithoutRecovery") {
			_ = a.db.del("secret", s.Name)
			a.untagAll(s.Arn)
			return M{"ARN": s.Arn, "Name": s.Name, "DeletionDate": epoch(nowMs())}, nil
		}
		days := getInt(in, "RecoveryWindowInDays", 30)
		s.Deleted = time.Now().Add(time.Duration(days) * 24 * time.Hour).UnixMilli()
		_ = a.db.put("secret", s.Name, s)
		return M{"ARN": s.Arn, "Name": s.Name, "DeletionDate": epoch(s.Deleted)}, nil
	case "RestoreSecret":
		s, e := get()
		if e != nil {
			return nil, e
		}
		s.Deleted = 0
		_ = a.db.put("secret", s.Name, s)
		return M{"ARN": s.Arn, "Name": s.Name}, nil
	case "GetResourcePolicy", "PutResourcePolicy", "DeleteResourcePolicy":
		s, e := get()
		if e != nil {
			return nil, e
		}
		switch op {
		case "GetResourcePolicy":
			out := M{"ARN": s.Arn, "Name": s.Name}
			if s.Policy != "" {
				out["ResourcePolicy"] = s.Policy
			}
			return out, nil
		case "PutResourcePolicy":
			s.Policy = getStr(in, "ResourcePolicy")
		default:
			s.Policy = ""
		}
		_ = a.db.put("secret", s.Name, s)
		return M{"ARN": s.Arn, "Name": s.Name}, nil
	case "ValidateResourcePolicy":
		return M{"PolicyValidationPassed": true, "ValidationErrors": []any{}}, nil
	case "TagResource", "UntagResource":
		s, e := get()
		if e != nil {
			return nil, e
		}
		if op == "TagResource" {
			a.tagResource(s.Arn, tagsFromList(getList(in, "Tags"), "Key", "Value"))
		} else {
			a.untagResource(s.Arn, strList(getList(in, "TagKeys")))
		}
		return M{}, nil
	case "GetRandomPassword":
		return M{"RandomPassword": randomPassword(in)}, nil
	case "RotateSecret", "CancelRotateSecret":
		s, e := live()
		if e != nil {
			return nil, e
		}
		return M{"ARN": s.Arn, "Name": s.Name}, nil
	}
	return nil, errf(400, "InvalidRequestException", "localaws secretsmanager: %s is not implemented", op)
}

// secretValue resolves an ECS valueFrom: <secret arn>[:<json-key>[:<stage>[:<version id>]]].
func (a *App) secretValue(from string) (string, bool) {
	ref, rest := from, ""
	if parts := strings.SplitN(from, ":", 8); len(parts) == 8 {
		ref, rest = strings.Join(parts[:7], ":"), parts[7]
	}
	s, ok := a.secret(ref)
	if !ok || s.Deleted > 0 {
		return "", false
	}
	f := strings.Split(rest+"::", ":")
	key, stage, ver := f[0], f[1], f[2]
	if stage == "" {
		stage = "AWSCURRENT"
	}
	var v *SecretVersion
	for i := range s.Versions {
		if (ver != "" && s.Versions[i].ID == ver) || (ver == "" && contains(s.Versions[i].Stages, stage)) {
			v = &s.Versions[i]
		}
	}
	if v == nil {
		return "", false
	}
	if key == "" {
		return v.String, true
	}
	var doc map[string]any
	if json.Unmarshal([]byte(v.String), &doc) != nil {
		return "", false
	}
	val, ok := doc[key]
	return str(val), ok
}

func randAlnum(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		x, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[x.Int64()]
	}
	return string(b)
}

func randomPassword(in M) string {
	n := getInt(in, "PasswordLength", 32)
	sets := map[string]string{"lower": "abcdefghijklmnopqrstuvwxyz", "upper": "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "digit": "0123456789",
		"punct": "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"}
	for flag, k := range map[string]string{"ExcludeLowercase": "lower", "ExcludeUppercase": "upper", "ExcludeNumbers": "digit", "ExcludePunctuation": "punct"} {
		if getBool(in, flag) {
			delete(sets, k)
		}
	}
	var pool []byte
	for _, k := range sortedKeys(sets) {
		for _, c := range []byte(sets[k]) {
			if !strings.ContainsRune(getStr(in, "ExcludeCharacters"), rune(c)) {
				pool = append(pool, c)
			}
		}
	}
	if getBool(in, "IncludeSpace") {
		pool = append(pool, ' ')
	}
	if len(pool) == 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		x, _ := rand.Int(rand.Reader, big.NewInt(int64(len(pool))))
		b[i] = pool[x.Int64()]
	}
	return string(b)
}
