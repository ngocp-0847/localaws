package main

// Query-protocol services (form-encoded Action=…, XML answers): STS (who am I, assumed
// roles, session tokens), EC2 (the networks declared in the config — what ECS validates
// subnets against), SNS (topics; Publish is accepted and logged).

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

type Topic struct {
	Name    string `json:"name"`
	Created int64  `json:"created"`
}

func (a *App) creds() string {
	exp := time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)
	return "<Credentials><AccessKeyId>ASIA" + strings.ToUpper(randHex(8)) + "</AccessKeyId><SecretAccessKey>" + randHex(20) +
		"</SecretAccessKey><SessionToken>" + randHex(64) + "</SessionToken><Expiration>" + exp + "</Expiration></Credentials>"
}

func (a *App) queryAPI(service string, v url.Values) (string, *apiError) {
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
	case "GetSessionToken":
		return fmt.Sprintf("<GetSessionTokenResponse %s><GetSessionTokenResult>%s</GetSessionTokenResult>%s</GetSessionTokenResponse>", stsNS, a.creds(), meta), nil
	case "DescribeSubnets":
		var sb strings.Builder
		for _, s := range a.cfg.Resources.EC2.Subnets {
			sb.WriteString(fmt.Sprintf("<item><subnetId>%s</subnetId><state>available</state><vpcId>%s</vpcId><cidrBlock>%s</cidrBlock><availabilityZone>%s</availabilityZone>%s</item>",
				xe(s.ID), xe(s.VpcID), xe(s.Cidr), xe(s.AZ), tagSet(s.Tags)))
		}
		return "<DescribeSubnetsResponse " + ec2NS + "><requestId>" + uuid4() + "</requestId><subnetSet>" + sb.String() + "</subnetSet></DescribeSubnetsResponse>", nil
	case "DescribeSecurityGroups":
		var sb strings.Builder
		for _, g := range a.cfg.Resources.EC2.SecurityGroups {
			sb.WriteString(fmt.Sprintf("<item><groupId>%s</groupId><groupName>%s</groupName><groupDescription>%s</groupDescription><vpcId>%s</vpcId>%s</item>",
				xe(g.ID), xe(g.Name), xe(g.Name), xe(g.VpcID), tagSet(g.Tags)))
		}
		return "<DescribeSecurityGroupsResponse " + ec2NS + "><requestId>" + uuid4() + "</requestId><securityGroupInfo>" + sb.String() + "</securityGroupInfo></DescribeSecurityGroupsResponse>", nil
	case "DescribeVpcs":
		seen := map[string]bool{}
		var sb strings.Builder
		for _, s := range a.cfg.Resources.EC2.Subnets {
			if s.VpcID != "" && !seen[s.VpcID] {
				seen[s.VpcID] = true
				sb.WriteString("<item><vpcId>" + xe(s.VpcID) + "</vpcId><state>available</state><cidrBlock>10.0.0.0/16</cidrBlock></item>")
			}
		}
		return "<DescribeVpcsResponse " + ec2NS + "><requestId>" + uuid4() + "</requestId><vpcSet>" + sb.String() + "</vpcSet></DescribeVpcsResponse>", nil
	case "DescribeInstances":
		return "<DescribeInstancesResponse " + ec2NS + "><requestId>" + uuid4() + "</requestId><reservationSet/></DescribeInstancesResponse>", nil
	case "DescribeRegions":
		return "<DescribeRegionsResponse " + ec2NS + "><requestId>" + uuid4() + "</requestId><regionInfo><item><regionName>" + a.cfg.Region +
			"</regionName><regionEndpoint>" + xe(a.cfg.PublicURL) + "</regionEndpoint></item></regionInfo></DescribeRegionsResponse>", nil
	case "CreateTopic":
		name := v.Get("Name")
		if !a.db.has("topic", name) {
			_ = a.db.put("topic", name, Topic{Name: name, Created: nowMs()})
		}
		return "<CreateTopicResponse " + snsNS + "><CreateTopicResult><TopicArn>" + a.arn("sns", name) + "</TopicArn></CreateTopicResult>" + meta + "</CreateTopicResponse>", nil
	case "ListTopics":
		var sb strings.Builder
		for _, t := range list[Topic](a.db, "topic") {
			sb.WriteString("<member><TopicArn>" + a.arn("sns", t.Name) + "</TopicArn></member>")
		}
		return "<ListTopicsResponse " + snsNS + "><ListTopicsResult><Topics>" + sb.String() + "</Topics></ListTopicsResult>" + meta + "</ListTopicsResponse>", nil
	case "DeleteTopic":
		_ = a.db.del("topic", arnTail(v.Get("TopicArn")))
		return "<DeleteTopicResponse " + snsNS + ">" + meta + "</DeleteTopicResponse>", nil
	case "Publish":
		topic := arnTail(v.Get("TopicArn"))
		if !a.db.has("topic", topic) {
			return "", errf(404, "NotFound", "Topic does not exist")
		}
		a.logf("sns publish to %s (accepted, %d bytes; no subscriptions are emulated)", topic, len(v.Get("Message")))
		return "<PublishResponse " + snsNS + "><PublishResult><MessageId>" + uuid4() + "</MessageId></PublishResult>" + meta + "</PublishResponse>", nil
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
