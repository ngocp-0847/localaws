package main

// KMS (JSON, TrentService.*): keys, aliases, key policies, rotation, enable / disable,
// scheduled deletion, tags — and Encrypt / Decrypt / GenerateDataKey with a reversible
// envelope (the ciphertext names its key; nothing is secret here). Enough for keys that
// other resources reference (SSM SecureString, S3 SSE-KMS, log groups, secrets) and for
// code that encrypts and decrypts through KMS.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"time"
)

type KMSKey struct {
	ID, Description, Usage, Spec, Policy, State, Origin string
	Enabled, Rotation, MultiRegion                      bool
	RotationDays                                        int
	Created, DeleteAt                                   int64
}

type KMSAlias struct {
	Name, Target   string
	Created, Moved int64
}

func (a *App) kmsKeyArn(id string) string { return a.arn("kms", "key/"+id) }

func (a *App) defaultKeyPolicy() string {
	return jsonStr(M{"Version": "2012-10-17", "Id": "key-default-1", "Statement": []any{M{"Sid": "Enable IAM User Permissions", "Effect": "Allow",
		"Principal": M{"AWS": "arn:aws:iam::" + a.cfg.Account + ":root"}, "Action": "kms:*", "Resource": "*"}}})
}

// kmsKey resolves a key id, key ARN, alias name or alias ARN.
func (a *App) kmsKey(ref string) (KMSKey, *apiError) {
	var k KMSKey
	id := ref
	if i := strings.Index(ref, ":key/"); i >= 0 {
		id = ref[i+5:]
	}
	if i := strings.Index(ref, ":alias/"); i >= 0 {
		id = ref[i+1:]
	}
	if strings.HasPrefix(id, "alias/") {
		var al KMSAlias
		if !a.db.get("kmsalias", id, &al) {
			if strings.HasPrefix(id, "alias/aws/") { // AWS managed keys exist on first use
				al = KMSAlias{Name: id, Target: uuid4(), Created: nowMs()}
				_ = a.db.put("kmskey", al.Target, KMSKey{ID: al.Target, Description: "Default key that protects my " + strings.TrimPrefix(id, "alias/aws/") + " data",
					Usage: "ENCRYPT_DECRYPT", Spec: "SYMMETRIC_DEFAULT", Policy: a.defaultKeyPolicy(), State: "Enabled", Origin: "AWS_KMS", Enabled: true, Created: nowMs()})
				_ = a.db.put("kmsalias", id, al)
			} else {
				return k, errf(400, "NotFoundException", "Alias %s is not found.", a.arn("kms", id))
			}
		}
		id = al.Target
	}
	if !a.db.get("kmskey", id, &k) {
		return k, errf(400, "NotFoundException", "Key '%s' does not exist", a.kmsKeyArn(id))
	}
	return k, nil
}

func (a *App) keyMeta(k KMSKey) M {
	m := M{"AWSAccountId": a.cfg.Account, "KeyId": k.ID, "Arn": a.kmsKeyArn(k.ID), "CreationDate": epoch(k.Created), "Enabled": k.Enabled,
		"Description": k.Description, "KeyUsage": k.Usage, "KeyState": k.State, "Origin": k.Origin, "KeyManager": "CUSTOMER",
		"CustomerMasterKeySpec": k.Spec, "KeySpec": k.Spec, "MultiRegion": k.MultiRegion}
	switch {
	case k.Usage == "ENCRYPT_DECRYPT" && k.Spec == "SYMMETRIC_DEFAULT":
		m["EncryptionAlgorithms"] = []any{"SYMMETRIC_DEFAULT"}
	case k.Usage == "ENCRYPT_DECRYPT":
		m["EncryptionAlgorithms"] = []any{"RSAES_OAEP_SHA_1", "RSAES_OAEP_SHA_256"}
	case k.Usage == "SIGN_VERIFY":
		m["SigningAlgorithms"] = []any{"RSASSA_PKCS1_V1_5_SHA_256"}
	case k.Usage == "GENERATE_VERIFY_MAC":
		m["MacAlgorithms"] = []any{"HMAC_SHA_256"}
	}
	if k.MultiRegion {
		m["MultiRegionConfiguration"] = M{"MultiRegionKeyType": "PRIMARY", "PrimaryKey": M{"Arn": a.kmsKeyArn(k.ID), "Region": a.cfg.Region}, "ReplicaKeys": []any{}}
	}
	if k.DeleteAt > 0 {
		m["DeletionDate"] = epoch(k.DeleteAt)
	}
	return m
}

// envelope: "lkms1" + 36-byte key id + plaintext.
func kmsSeal(keyID string, plain []byte) string {
	return base64.StdEncoding.EncodeToString(append([]byte("lkms1"+keyID), plain...))
}

func kmsOpen(blob string) (string, []byte, bool) {
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil || len(raw) < 41 || !bytes.HasPrefix(raw, []byte("lkms1")) {
		return "", nil, false
	}
	return string(raw[5:41]), raw[41:], true
}

func (a *App) kmsAPI(op string, in M) (any, *apiError) {
	keyOf := func() (KMSKey, *apiError) { return a.kmsKey(getStr(in, "KeyId")) }
	usable := func(k KMSKey) *apiError {
		if !k.Enabled || k.State != "Enabled" {
			return errf(400, "DisabledException", "%s is %s.", a.kmsKeyArn(k.ID), strings.ToLower(k.State))
		}
		return nil
	}
	switch op {
	case "CreateKey":
		k := KMSKey{ID: uuid4(), Description: getStr(in, "Description"), Usage: getStr(in, "KeyUsage"), Spec: getStr(in, "KeySpec"),
			Policy: getStr(in, "Policy"), State: "Enabled", Origin: getStr(in, "Origin"), Enabled: true, MultiRegion: getBool(in, "MultiRegion"), Created: nowMs()}
		if k.Usage == "" {
			k.Usage = "ENCRYPT_DECRYPT"
		}
		if k.Spec == "" {
			k.Spec = getStr(in, "CustomerMasterKeySpec")
		}
		if k.Spec == "" {
			k.Spec = "SYMMETRIC_DEFAULT"
		}
		if k.Policy == "" {
			k.Policy = a.defaultKeyPolicy()
		}
		if k.Origin == "" {
			k.Origin = "AWS_KMS"
		}
		_ = a.db.put("kmskey", k.ID, k)
		a.tagResource(a.kmsKeyArn(k.ID), tagsFromList(getList(in, "Tags"), "TagKey", "TagValue"))
		return M{"KeyMetadata": a.keyMeta(k)}, nil
	case "DescribeKey":
		k, e := keyOf()
		if e != nil {
			return nil, e
		}
		return M{"KeyMetadata": a.keyMeta(k)}, nil
	case "ListKeys":
		out := []any{}
		for _, k := range list[KMSKey](a.db, "kmskey") {
			out = append(out, M{"KeyId": k.ID, "KeyArn": a.kmsKeyArn(k.ID)})
		}
		return M{"Keys": out, "Truncated": false}, nil
	case "GetKeyPolicy", "PutKeyPolicy", "ListKeyPolicies":
		k, e := keyOf()
		if e != nil {
			return nil, e
		}
		switch op {
		case "GetKeyPolicy":
			return M{"Policy": k.Policy, "PolicyName": "default"}, nil
		case "ListKeyPolicies":
			return M{"PolicyNames": []any{"default"}, "Truncated": false}, nil
		}
		k.Policy = getStr(in, "Policy")
		_ = a.db.put("kmskey", k.ID, k)
		return M{}, nil
	case "GetKeyRotationStatus":
		k, e := keyOf()
		if e != nil {
			return nil, e
		}
		out := M{"KeyRotationEnabled": k.Rotation, "KeyId": a.kmsKeyArn(k.ID)}
		if k.Rotation {
			days := k.RotationDays
			if days == 0 {
				days = 365
			}
			out["RotationPeriodInDays"] = days
			out["NextRotationDate"] = epoch(k.Created + int64(days)*86400000)
		}
		return out, nil
	case "EnableKeyRotation", "DisableKeyRotation", "EnableKey", "DisableKey", "UpdateKeyDescription", "CancelKeyDeletion":
		k, e := keyOf()
		if e != nil {
			return nil, e
		}
		switch op {
		case "EnableKeyRotation":
			k.Rotation, k.RotationDays = true, getInt(in, "RotationPeriodInDays", 365)
		case "DisableKeyRotation":
			k.Rotation = false
		case "EnableKey":
			k.Enabled, k.State = true, "Enabled"
		case "DisableKey":
			k.Enabled, k.State = false, "Disabled"
		case "UpdateKeyDescription":
			k.Description = getStr(in, "Description")
		case "CancelKeyDeletion":
			k.State, k.DeleteAt = "Disabled", 0
		}
		_ = a.db.put("kmskey", k.ID, k)
		if op == "CancelKeyDeletion" {
			return M{"KeyId": a.kmsKeyArn(k.ID)}, nil
		}
		return M{}, nil
	case "ScheduleKeyDeletion":
		k, e := keyOf()
		if e != nil {
			return nil, e
		}
		days := getInt(in, "PendingWindowInDays", 30)
		k.State, k.Enabled, k.DeleteAt = "PendingDeletion", false, time.Now().Add(time.Duration(days)*24*time.Hour).UnixMilli()
		_ = a.db.put("kmskey", k.ID, k)
		return M{"KeyId": a.kmsKeyArn(k.ID), "DeletionDate": epoch(k.DeleteAt), "KeyState": k.State, "PendingWindowInDays": days}, nil
	case "CreateAlias", "UpdateAlias":
		name := getStr(in, "AliasName")
		k, e := a.kmsKey(getStr(in, "TargetKeyId"))
		if e != nil {
			return nil, e
		}
		var al KMSAlias
		exists := a.db.get("kmsalias", name, &al)
		if op == "CreateAlias" && exists {
			return nil, errf(400, "AlreadyExistsException", "An alias with the name %s already exists", a.arn("kms", name))
		}
		if op == "UpdateAlias" && !exists {
			return nil, errf(400, "NotFoundException", "Alias %s is not found.", a.arn("kms", name))
		}
		if !exists {
			al.Created = nowMs()
		}
		al.Name, al.Target, al.Moved = name, k.ID, nowMs()
		_ = a.db.put("kmsalias", name, al)
		return M{}, nil
	case "DeleteAlias":
		if !a.db.has("kmsalias", getStr(in, "AliasName")) {
			return nil, errf(400, "NotFoundException", "Alias %s is not found.", a.arn("kms", getStr(in, "AliasName")))
		}
		_ = a.db.del("kmsalias", getStr(in, "AliasName"))
		return M{}, nil
	case "ListAliases":
		out := []any{}
		want := ""
		if ref := getStr(in, "KeyId"); ref != "" {
			k, e := a.kmsKey(ref)
			if e != nil {
				return nil, e
			}
			want = k.ID
		}
		for _, al := range list[KMSAlias](a.db, "kmsalias") {
			if want == "" || al.Target == want {
				out = append(out, M{"AliasName": al.Name, "AliasArn": a.arn("kms", al.Name), "TargetKeyId": al.Target,
					"CreationDate": epoch(al.Created), "LastUpdatedDate": epoch(al.Moved)})
			}
		}
		return M{"Aliases": out, "Truncated": false}, nil
	case "TagResource", "UntagResource", "ListResourceTags":
		k, e := keyOf()
		if e != nil {
			return nil, e
		}
		arn := a.kmsKeyArn(k.ID)
		switch op {
		case "TagResource":
			a.tagResource(arn, tagsFromList(getList(in, "Tags"), "TagKey", "TagValue"))
		case "UntagResource":
			a.untagResource(arn, strList(getList(in, "TagKeys")))
		default:
			return M{"Tags": tagList(a.tagsOf(arn), "TagKey", "TagValue"), "Truncated": false}, nil
		}
		return M{}, nil
	case "Encrypt":
		k, e := keyOf()
		if e != nil {
			return nil, e
		}
		if e := usable(k); e != nil {
			return nil, e
		}
		plain, _ := base64.StdEncoding.DecodeString(getStr(in, "Plaintext"))
		return M{"CiphertextBlob": kmsSeal(k.ID, plain), "KeyId": a.kmsKeyArn(k.ID), "EncryptionAlgorithm": "SYMMETRIC_DEFAULT"}, nil
	case "Decrypt", "ReEncrypt":
		id, plain, ok := kmsOpen(getStr(in, "CiphertextBlob"))
		if !ok {
			return nil, errf(400, "InvalidCiphertextException", "The ciphertext is not valid.")
		}
		k, e := a.kmsKey(id)
		if e != nil {
			return nil, e
		}
		if e := usable(k); e != nil {
			return nil, e
		}
		if op == "ReEncrypt" {
			dst, e := a.kmsKey(getStr(in, "DestinationKeyId"))
			if e != nil {
				return nil, e
			}
			return M{"CiphertextBlob": kmsSeal(dst.ID, plain), "KeyId": a.kmsKeyArn(dst.ID), "SourceKeyId": a.kmsKeyArn(k.ID)}, nil
		}
		return M{"Plaintext": base64.StdEncoding.EncodeToString(plain), "KeyId": a.kmsKeyArn(k.ID), "EncryptionAlgorithm": "SYMMETRIC_DEFAULT"}, nil
	case "GenerateDataKey", "GenerateDataKeyWithoutPlaintext":
		k, e := keyOf()
		if e != nil {
			return nil, e
		}
		if e := usable(k); e != nil {
			return nil, e
		}
		n := getInt(in, "NumberOfBytes", 32)
		if getStr(in, "KeySpec") == "AES_128" {
			n = 16
		}
		plain := make([]byte, n)
		_, _ = rand.Read(plain)
		out := M{"CiphertextBlob": kmsSeal(k.ID, plain), "KeyId": a.kmsKeyArn(k.ID)}
		if op == "GenerateDataKey" {
			out["Plaintext"] = base64.StdEncoding.EncodeToString(plain)
		}
		return out, nil
	case "GenerateRandom":
		b := make([]byte, getInt(in, "NumberOfBytes", 32))
		_, _ = rand.Read(b)
		return M{"Plaintext": base64.StdEncoding.EncodeToString(b)}, nil
	case "ListGrants", "ListRetirableGrants":
		return M{"Grants": []any{}, "Truncated": false}, nil
	}
	return nil, errf(400, "UnsupportedOperationException", "localaws kms: %s is not implemented", op)
}
