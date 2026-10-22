package main

// Query-protocol services (form-encoded Action=…, XML answers), routed by the SigV4
// signing scope: STS here (who am I, assumed roles, session tokens); EC2 (ec2.go),
// SNS (sns.go), IAM (iam.go) and SES (ses.go) in their own files.

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	stsNS = `xmlns="https://sts.amazonaws.com/doc/2011-06-15/"`
	ec2NS = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	snsNS = `xmlns="http://sns.amazonaws.com/doc/2010-03-31/"`
)

func (a *App) creds() string {
	exp := time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)
	return "<Credentials><AccessKeyId>ASIA" + strings.ToUpper(randHex(8)) + "</AccessKeyId><SecretAccessKey>" + randHex(20) +
		"</SecretAccessKey><SessionToken>" + randHex(64) + "</SessionToken><Expiration>" + exp + "</Expiration></Credentials>"
}

func (a *App) queryAPI(service string, v url.Values) (string, *apiError) {
	switch service {
	case "ses", "email":
		return a.sesQuery(v)
	case "sns":
		return a.snsQuery(v)
	case "ec2":
		return a.ec2Query(v)
	case "iam":
		return a.iamQuery(v)
	}
	action := v.Get("Action")
	meta := "<ResponseMetadata><RequestId>" + uuid4() + "</RequestId></ResponseMetadata>"
	switch action {
	case "GetCallerIdentity":
		return fmt.Sprintf("<GetCallerIdentityResponse %s><GetCallerIdentityResult><Arn>%s</Arn><UserId>AIDA%s</UserId><Account>%s</Account></GetCallerIdentityResult>%s</GetCallerIdentityResponse>",
			stsNS, xe(a.cfg.IdentityArn()), strings.ToUpper(randHex(8)), a.cfg.Account, meta), nil
	case "AssumeRole":
		role, session := v.Get("RoleArn"), v.Get("RoleSessionName")
		assumed := strings.Replace(strings.Replace(role, ":iam::", ":sts::", 1), ":role/", ":assumed-role/", 1) + "/" + session
		return fmt.Sprintf("<AssumeRoleResponse %s><AssumeRoleResult>%s<AssumedRoleUser><Arn>%s</Arn><AssumedRoleId>AROA%s:%s</AssumedRoleId></AssumedRoleUser></AssumeRoleResult>%s</AssumeRoleResponse>",
			stsNS, a.creds(), xe(assumed), strings.ToUpper(randHex(8)), xe(session), meta), nil
	case "AssumeRoleWithWebIdentity":
		role, session := v.Get("RoleArn"), v.Get("RoleSessionName")
		assumed := strings.Replace(strings.Replace(role, ":iam::", ":sts::", 1), ":role/", ":assumed-role/", 1) + "/" + session
		return fmt.Sprintf("<AssumeRoleWithWebIdentityResponse %s><AssumeRoleWithWebIdentityResult>%s<AssumedRoleUser><Arn>%s</Arn><AssumedRoleId>AROA%s:%s</AssumedRoleId></AssumedRoleUser></AssumeRoleWithWebIdentityResult>%s</AssumeRoleWithWebIdentityResponse>",
			stsNS, a.creds(), xe(assumed), strings.ToUpper(randHex(8)), xe(session), meta), nil
	case "GetSessionToken":
		return fmt.Sprintf("<GetSessionTokenResponse %s><GetSessionTokenResult>%s</GetSessionTokenResult>%s</GetSessionTokenResponse>", stsNS, a.creds(), meta), nil
	}
	return "", errf(400, "InvalidAction", "localaws %s: action %s is not implemented", service, action)
}

func tagSet(tags map[string]string) string {
	if len(tags) == 0 {
		return "<tagSet/>"
	}
	var sb strings.Builder
	sb.WriteString("<tagSet>")
	for _, k := range sortedKeys(tags) {
		sb.WriteString("<item><key>" + xe(k) + "</key><value>" + xe(tags[k]) + "</value></item>")
	}
	sb.WriteString("</tagSet>")
	return sb.String()
}
