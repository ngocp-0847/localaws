package main

// EC2 networking (Query protocol, XML answers): VPCs (with their main route table,
// default security group and default network ACL), subnets, security groups and their
// rules, route tables, internet / NAT gateways, Elastic IPs, availability zones, tags.
// Nothing is routed — these are records, kept so that infrastructure code (Terraform,
// CDK, scripts) can create them and read them back, and so ECS can validate awsvpc
// network configurations against the subnets that exist. Subnets and security groups
// declared in the config file (resources.ec2) are records here too.

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type Vpc struct {
	ID           string `json:"id"`
	Cidr         string `json:"cidr"`
	Tenancy      string `json:"tenancy"`
	DNSSupport   bool   `json:"dnsSupport"`
	DNSHostnames bool   `json:"dnsHostnames"`
	NAUMetrics   bool   `json:"nauMetrics"`
	Created      int64  `json:"created"`
}

type EC2Subnet struct {
	ID           string `json:"id"`
	VpcID        string `json:"vpcId"`
	Cidr         string `json:"cidr"`
	AZ           string `json:"az"`
	MapPublicIP  bool   `json:"mapPublicIp"`
	HostnameType string `json:"hostnameType"`
	DNSA         bool   `json:"dnsA"`
	DNSAAAA      bool   `json:"dnsAAAA"`
	Created      int64  `json:"created"`
}

// SGRule is one rule as the security-group-rules API sees it: one source per rule.
type SGRule struct {
	ID         string `json:"id"`
	Egress     bool   `json:"egress"`
	Protocol   string `json:"protocol"`
	From       int    `json:"from"`
	To         int    `json:"to"`
	Cidr       string `json:"cidr,omitempty"`
	Cidr6      string `json:"cidr6,omitempty"`
	Group      string `json:"group,omitempty"`
	PrefixList string `json:"prefixList,omitempty"`
	Desc       string `json:"desc,omitempty"`
}

type SecGroup struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Desc  string   `json:"desc"`
	VpcID string   `json:"vpcId"`
	Rules []SGRule `json:"rules"`
}

type Route struct {
	Dest, Dest6, PrefixList, Gateway, NatGateway, Endpoint, Eni, TransitGateway, Peering, Origin string
}

type RTAssoc struct {
	ID, Subnet, Gateway string
	Main                bool
}

type RouteTable struct {
	ID     string    `json:"id"`
	VpcID  string    `json:"vpcId"`
	Routes []Route   `json:"routes"`
	Assocs []RTAssoc `json:"assocs"`
}

type IGW struct {
	ID    string `json:"id"`
	VpcID string `json:"vpcId,omitempty"`
}

type NatGW struct {
	ID, SubnetID, VpcID, AllocationID, Connectivity, State, PrivateIP string
	Created                                                           int64
}

type EIP struct {
	ID, PublicIP, Domain, Association, Instance, Eni string
}

func ec2ID(prefix string) string { return prefix + "-" + randHex(9)[:17] }

// ── request helpers ──────────────────────────────────────────────────────

// listN reads a flattened Query list: <prefix>.1, <prefix>.2, …
func listN(v url.Values, prefix string) []string {
	var out []string
	for i := 1; ; i++ {
		s, ok := v[prefix+"."+strconv.Itoa(i)]
		if !ok {
			return out
		}
		out = append(out, s[0])
	}
}

type ec2Filter struct {
	Name   string
	Values []string
}

func ec2Filters(v url.Values) []ec2Filter {
	var out []ec2Filter
	for i := 1; ; i++ {
		p := "Filter." + strconv.Itoa(i)
		n, ok := v[p+".Name"]
		if !ok {
			return out
		}
		out = append(out, ec2Filter{n[0], listN(v, p+".Value")})
	}
}

// ec2Match: every filter must match one of its values (with * and ? wildcards) against
// the resource's fields; tag:<key> and tag-key filters look at its tags.
func ec2Match(fields map[string][]string, tags map[string]string, filters []ec2Filter) bool {
	for _, f := range filters {
		var have []string
		switch {
		case strings.HasPrefix(f.Name, "tag:"):
			if v, ok := tags[f.Name[4:]]; ok {
				have = []string{v}
			}
		case f.Name == "tag-key":
			have = sortedKeys(tags)
		case f.Name == "tag-value":
			for _, k := range sortedKeys(tags) {
				have = append(have, tags[k])
			}
		default:
			var known bool
			have, known = fields[f.Name]
			if !known {
				continue // a filter this emulator does not know narrows nothing
			}
		}
		hit := false
		for _, want := range f.Values {
			for _, h := range have {
				if wildcard(strings.ReplaceAll(want, "?", "*"), h) {
					hit = true
				}
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// tagSpecs reads TagSpecification.N.Tag.M.Key / .Value (all resource types).
func tagSpecs(v url.Values) map[string]string {
	out := map[string]string{}
	for i := 1; ; i++ {
		p := "TagSpecification." + strconv.Itoa(i)
		if _, ok := v[p+".ResourceType"]; !ok {
			if _, ok2 := v[p+".Tag.1.Key"]; !ok2 {
				return out
			}
		}
		for j := 1; ; j++ {
			k, ok := v[p+".Tag."+strconv.Itoa(j)+".Key"]
			if !ok {
				break
			}
			out[k[0]] = v.Get(p + ".Tag." + strconv.Itoa(j) + ".Value")
		}
	}
}

func boolS(b bool) string { return strconv.FormatBool(b) }

func ec2Err(code, format string, args ...any) *apiError {
	return errf(400, code, format, args...)
}

// ── seeding from the config file ─────────────────────────────────────────
func (a *App) seedEC2() {
	for _, s := range a.cfg.Resources.EC2.Subnets {
		if s.VpcID != "" && !a.db.has("vpc", s.VpcID) {
			a.newVpc(Vpc{ID: s.VpcID, Cidr: "10.0.0.0/16", Tenancy: "default", DNSSupport: true})
		}
		var cur EC2Subnet
		if !a.db.get("subnet", s.ID, &cur) {
			cur = EC2Subnet{ID: s.ID, HostnameType: "ip-name", Created: nowMs()}
		}
		cur.VpcID, cur.Cidr, cur.AZ = s.VpcID, s.Cidr, s.AZ
		if cur.AZ == "" {
			cur.AZ = a.cfg.Region + "a"
		}
		_ = a.db.put("subnet", s.ID, cur)
		a.tagResource(s.ID, s.Tags)
	}
	for _, g := range a.cfg.Resources.EC2.SecurityGroups {
		if g.VpcID != "" && !a.db.has("vpc", g.VpcID) {
			a.newVpc(Vpc{ID: g.VpcID, Cidr: "10.0.0.0/16", Tenancy: "default", DNSSupport: true})
		}
		var cur SecGroup
		if !a.db.get("sg", g.ID, &cur) {
			cur = SecGroup{ID: g.ID, Rules: []SGRule{{ID: ec2ID("sgr"), Egress: true, Protocol: "-1", From: -1, To: -1, Cidr: "0.0.0.0/0"}}}
		}
		cur.Name, cur.VpcID = g.Name, g.VpcID
		if cur.Name == "" {
			cur.Name = g.ID
		}
		cur.Desc = cur.Name
		_ = a.db.put("sg", g.ID, cur)
		a.tagResource(g.ID, g.Tags)
	}
}

// subnetIDs: what awsvpc RunTask calls are validated against (nil: no subnets at all —
// then anything goes, as before networks were declared).
func (a *App) subnetIDs() map[string]bool {
	subs := list[EC2Subnet](a.db, "subnet")
	if len(subs) == 0 {
		return nil
	}
	m := map[string]bool{}
	for _, s := range subs {
		m[s.ID] = true
	}
	return m
}

// newVpc stores a VPC with what AWS creates alongside it: the main route table (with
// the local route), the default security group and (implicitly) the default network ACL.
func (a *App) newVpc(v Vpc) Vpc {
	if v.Created == 0 {
		v.Created = nowMs()
	}
	_ = a.db.put("vpc", v.ID, v)
	rt := RouteTable{ID: ec2ID("rtb"), VpcID: v.ID, Routes: []Route{{Dest: v.Cidr, Gateway: "local", Origin: "CreateRouteTable"}},
		Assocs: []RTAssoc{{ID: ec2ID("rtbassoc"), Main: true}}}
	_ = a.db.put("routetable", rt.ID, rt)
	sg := SecGroup{ID: ec2ID("sg"), Name: "default", Desc: "default VPC security group", VpcID: v.ID}
	sg.Rules = []SGRule{{ID: ec2ID("sgr"), Protocol: "-1", From: -1, To: -1, Group: sg.ID},
		{ID: ec2ID("sgr"), Egress: true, Protocol: "-1", From: -1, To: -1, Cidr: "0.0.0.0/0"}}
	_ = a.db.put("sg", sg.ID, sg)
	return v
}

func defaultACL(vpcID string) string { return "acl-" + strings.TrimPrefix(vpcID, "vpc-") }

// ── rendering ────────────────────────────────────────────────────────────
func (a *App) vpcXML(v Vpc) string {
	return "<vpcId>" + v.ID + "</vpcId><ownerId>" + a.cfg.Account + "</ownerId><state>available</state><cidrBlock>" + xe(v.Cidr) +
		"</cidrBlock><cidrBlockAssociationSet><item><cidrBlock>" + xe(v.Cidr) + "</cidrBlock><associationId>vpc-cidr-assoc-" + strings.TrimPrefix(v.ID, "vpc-") +
		"</associationId><cidrBlockState><state>associated</state></cidrBlockState></item></cidrBlockAssociationSet><ipv6CidrBlockAssociationSet/>" +
		"<dhcpOptionsId>dopt-localaws</dhcpOptionsId><instanceTenancy>" + v.Tenancy + "</instanceTenancy><isDefault>false</isDefault>" + tagSet(a.tagsOf(v.ID))
}

func (a *App) azID(az string) string {
	region := strings.Split(a.cfg.Region, "-")
	short := ""
	for _, p := range region {
		if p != "" {
			short += p[:1]
		}
	}
	if len(region) == 3 {
		short = region[0] + map[string]string{"northeast": "ne", "southeast": "se", "northwest": "nw", "southwest": "sw", "east": "e", "west": "w",
			"central": "c", "north": "n", "south": "s"}[region[1]] + region[2]
	}
	n := 1
	if s := strings.TrimPrefix(az, a.cfg.Region); len(s) == 1 {
		n = int(s[0]-'a') + 1
	}
	return fmt.Sprintf("%s-az%d", short, n)
}

func (a *App) subnetXML(s EC2Subnet) string {
	return "<subnetId>" + s.ID + "</subnetId><subnetArn>" + a.arn("ec2", "subnet/"+s.ID) + "</subnetArn><state>available</state><ownerId>" + a.cfg.Account +
		"</ownerId><vpcId>" + s.VpcID + "</vpcId><cidrBlock>" + xe(s.Cidr) + "</cidrBlock><ipv6CidrBlockAssociationSet/><availableIpAddressCount>251</availableIpAddressCount>" +
		"<availabilityZone>" + s.AZ + "</availabilityZone><availabilityZoneId>" + a.azID(s.AZ) + "</availabilityZoneId><defaultForAz>false</defaultForAz>" +
		"<mapPublicIpOnLaunch>" + boolS(s.MapPublicIP) + "</mapPublicIpOnLaunch><mapCustomerOwnedIpOnLaunch>false</mapCustomerOwnedIpOnLaunch>" +
		"<assignIpv6AddressOnCreation>false</assignIpv6AddressOnCreation><enableDns64>false</enableDns64><ipv6Native>false</ipv6Native>" +
		"<enableLniAtDeviceIndex>0</enableLniAtDeviceIndex><privateDnsNameOptionsOnLaunch><hostnameType>" + s.HostnameType + "</hostnameType><enableResourceNameDnsARecord>" +
		boolS(s.DNSA) + "</enableResourceNameDnsARecord><enableResourceNameDnsAAAARecord>" + boolS(s.DNSAAAA) +
		"</enableResourceNameDnsAAAARecord></privateDnsNameOptionsOnLaunch>" + tagSet(a.tagsOf(s.ID))
}

func portsXML(r SGRule) string {
	if r.Protocol == "-1" {
		return ""
	}
	return "<fromPort>" + strconv.Itoa(r.From) + "</fromPort><toPort>" + strconv.Itoa(r.To) + "</toPort>"
}

func descXML(d string) string {
	if d == "" {
		return ""
	}
	return "<description>" + xe(d) + "</description>"
}

// permissionsXML groups rules by protocol and ports, as DescribeSecurityGroups does.
func (a *App) permissionsXML(rules []SGRule, egress bool) string {
	type key struct {
		p        string
		from, to int
	}
	groups := map[key][]SGRule{}
	var order []key
	for _, r := range rules {
		if r.Egress != egress {
			continue
		}
		k := key{r.Protocol, r.From, r.To}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	var sb strings.Builder
	for _, k := range order {
		var cidr, cidr6, grp, pl strings.Builder
		for _, r := range groups[k] {
			switch {
			case r.Cidr != "":
				cidr.WriteString("<item><cidrIp>" + r.Cidr + "</cidrIp>" + descXML(r.Desc) + "</item>")
			case r.Cidr6 != "":
				cidr6.WriteString("<item><cidrIpv6>" + r.Cidr6 + "</cidrIpv6>" + descXML(r.Desc) + "</item>")
			case r.Group != "":
				grp.WriteString("<item><userId>" + a.cfg.Account + "</userId><groupId>" + r.Group + "</groupId>" + descXML(r.Desc) + "</item>")
			case r.PrefixList != "":
				pl.WriteString("<item><prefixListId>" + r.PrefixList + "</prefixListId>" + descXML(r.Desc) + "</item>")
			}
		}
		sb.WriteString("<item><ipProtocol>" + k.p + "</ipProtocol>" + portsXML(groups[k][0]) + "<groups>" + grp.String() + "</groups><ipRanges>" +
			cidr.String() + "</ipRanges><ipv6Ranges>" + cidr6.String() + "</ipv6Ranges><prefixListIds>" + pl.String() + "</prefixListIds></item>")
	}
	return sb.String()
}

func (a *App) sgXML(g SecGroup) string {
	return "<ownerId>" + a.cfg.Account + "</ownerId><groupId>" + g.ID + "</groupId><groupName>" + xe(g.Name) + "</groupName><groupDescription>" +
		xe(g.Desc) + "</groupDescription><vpcId>" + g.VpcID + "</vpcId><securityGroupArn>" + a.arn("ec2", "security-group/"+g.ID) +
		"</securityGroupArn><ipPermissions>" + a.permissionsXML(g.Rules, false) + "</ipPermissions><ipPermissionsEgress>" +
		a.permissionsXML(g.Rules, true) + "</ipPermissionsEgress>" + tagSet(a.tagsOf(g.ID))
}

func (a *App) ruleXML(g SecGroup, r SGRule) string {
	from, to := r.From, r.To
	if r.Protocol == "-1" {
		from, to = -1, -1
	}
	s := "<securityGroupRuleId>" + r.ID + "</securityGroupRuleId><groupId>" + g.ID + "</groupId><groupOwnerId>" + a.cfg.Account +
		"</groupOwnerId><isEgress>" + boolS(r.Egress) + "</isEgress><ipProtocol>" + r.Protocol + "</ipProtocol><fromPort>" + strconv.Itoa(from) +
		"</fromPort><toPort>" + strconv.Itoa(to) + "</toPort><securityGroupRuleArn>" + a.arn("ec2", "security-group-rule/"+r.ID) + "</securityGroupRuleArn>"
	switch {
	case r.Cidr != "":
		s += "<cidrIpv4>" + r.Cidr + "</cidrIpv4>"
	case r.Cidr6 != "":
		s += "<cidrIpv6>" + r.Cidr6 + "</cidrIpv6>"
	case r.Group != "":
		s += "<referencedGroupInfo><groupId>" + r.Group + "</groupId><userId>" + a.cfg.Account + "</userId></referencedGroupInfo>"
	case r.PrefixList != "":
		s += "<prefixListId>" + r.PrefixList + "</prefixListId>"
	}
	return s + descXML(r.Desc) + tagSet(a.tagsOf(r.ID))
}

func (a *App) rtXML(rt RouteTable) string {
	var routes, assocs strings.Builder
	for _, r := range rt.Routes {
		routes.WriteString("<item>")
		for _, f := range [][2]string{{"destinationCidrBlock", r.Dest}, {"destinationIpv6CidrBlock", r.Dest6}, {"destinationPrefixListId", r.PrefixList},
			{"gatewayId", r.Gateway}, {"natGatewayId", r.NatGateway}, {"networkInterfaceId", r.Eni}, {"transitGatewayId", r.TransitGateway},
			{"vpcPeeringConnectionId", r.Peering}} {
			if f[1] != "" {
				routes.WriteString("<" + f[0] + ">" + xe(f[1]) + "</" + f[0] + ">")
			}
		}
		if r.Endpoint != "" {
			routes.WriteString("<gatewayId>" + r.Endpoint + "</gatewayId>")
		}
		routes.WriteString("<state>active</state><origin>" + r.Origin + "</origin></item>")
	}
	for _, as := range rt.Assocs {
		assocs.WriteString("<item><routeTableAssociationId>" + as.ID + "</routeTableAssociationId><routeTableId>" + rt.ID + "</routeTableId>")
		if as.Subnet != "" {
			assocs.WriteString("<subnetId>" + as.Subnet + "</subnetId>")
		}
		if as.Gateway != "" {
			assocs.WriteString("<gatewayId>" + as.Gateway + "</gatewayId>")
		}
		assocs.WriteString("<main>" + boolS(as.Main) + "</main><associationState><state>associated</state></associationState></item>")
	}
	return "<routeTableId>" + rt.ID + "</routeTableId><vpcId>" + rt.VpcID + "</vpcId><ownerId>" + a.cfg.Account + "</ownerId><routeSet>" +
		routes.String() + "</routeSet><associationSet>" + assocs.String() + "</associationSet><propagatingVgwSet/>" + tagSet(a.tagsOf(rt.ID))
}

func (a *App) igwXML(g IGW) string {
	att := ""
	if g.VpcID != "" {
		att = "<item><vpcId>" + g.VpcID + "</vpcId><state>available</state></item>"
	}
	return "<internetGatewayId>" + g.ID + "</internetGatewayId><ownerId>" + a.cfg.Account + "</ownerId><attachmentSet>" + att + "</attachmentSet>" + tagSet(a.tagsOf(g.ID))
}

func (a *App) natXML(n NatGW) string {
	var eip EIP
	a.db.get("eip", n.AllocationID, &eip)
	addr := "<natGatewayAddressSet><item><allocationId>" + n.AllocationID + "</allocationId><networkInterfaceId>eni-" + strings.TrimPrefix(n.ID, "nat-") +
		"</networkInterfaceId><privateIp>" + n.PrivateIP + "</privateIp>"
	if eip.PublicIP != "" {
		addr += "<publicIp>" + eip.PublicIP + "</publicIp>"
	}
	addr += "<isPrimary>true</isPrimary><status>succeeded</status></item></natGatewayAddressSet>"
	return "<natGatewayId>" + n.ID + "</natGatewayId><subnetId>" + n.SubnetID + "</subnetId><vpcId>" + n.VpcID + "</vpcId><state>" + n.State +
		"</state><connectivityType>" + n.Connectivity + "</connectivityType><createTime>" + isoMs(n.Created) + "</createTime>" + addr + tagSet(a.tagsOf(n.ID))
}

func (a *App) eipXML(e EIP) string {
	s := "<publicIp>" + e.PublicIP + "</publicIp><allocationId>" + e.ID + "</allocationId><domain>vpc</domain><publicIpv4Pool>amazon</publicIpv4Pool><networkBorderGroup>" + a.cfg.Region + "</networkBorderGroup>"
	if e.Association != "" {
		s += "<associationId>" + e.Association + "</associationId>"
	}
	if e.Eni != "" {
		s += "<networkInterfaceId>" + e.Eni + "</networkInterfaceId>"
	}
	return s + tagSet(a.tagsOf(e.ID))
}

// ── API ──────────────────────────────────────────────────────────────────
func (a *App) ec2Query(v url.Values) (string, *apiError) {
	action := v.Get("Action")
	resp := func(inner string) string {
		return "<" + action + "Response " + ec2NS + "><requestId>" + uuid4() + "</requestId>" + inner + "</" + action + "Response>"
	}
	ok := resp("<return>true</return>")
	filters := ec2Filters(v)
	created := func(id string) { a.tagResource(id, tagSpecs(v)) }

	switch action {
	// ── VPCs
	case "CreateVpc":
		cidr := v.Get("CidrBlock")
		if cidr == "" {
			cidr = "10.0.0.0/16"
		}
		tenancy := v.Get("InstanceTenancy")
		if tenancy == "" {
			tenancy = "default"
		}
		vpc := a.newVpc(Vpc{ID: ec2ID("vpc"), Cidr: cidr, Tenancy: tenancy, DNSSupport: true})
		created(vpc.ID)
		return resp("<vpc>" + a.vpcXML(vpc) + "</vpc>"), nil
	case "DescribeVpcs":
		ids := listN(v, "VpcId")
		var sb strings.Builder
		for _, id := range ids {
			if !a.db.has("vpc", id) {
				return "", ec2Err("InvalidVpcID.NotFound", "The vpc ID '%s' does not exist", id)
			}
		}
		for _, x := range list[Vpc](a.db, "vpc") {
			if (len(ids) > 0 && !contains(ids, x.ID)) || !ec2Match(map[string][]string{"vpc-id": {x.ID}, "cidr": {x.Cidr}, "cidr-block": {x.Cidr},
				"cidr-block-association.cidr-block": {x.Cidr}, "state": {"available"}, "is-default": {"false"}, "owner-id": {a.cfg.Account}}, a.tagsOf(x.ID), filters) {
				continue
			}
			sb.WriteString("<item>" + a.vpcXML(x) + "</item>")
		}
		return resp("<vpcSet>" + sb.String() + "</vpcSet>"), nil
	case "DescribeVpcAttribute":
		var x Vpc
		if !a.db.get("vpc", v.Get("VpcId"), &x) {
			return "", ec2Err("InvalidVpcID.NotFound", "The vpc ID '%s' does not exist", v.Get("VpcId"))
		}
		attr := v.Get("Attribute")
		val := map[string]bool{"enableDnsSupport": x.DNSSupport, "enableDnsHostnames": x.DNSHostnames, "enableNetworkAddressUsageMetrics": x.NAUMetrics}[attr]
		return resp("<vpcId>" + x.ID + "</vpcId><" + attr + "><value>" + boolS(val) + "</value></" + attr + ">"), nil
	case "ModifyVpcAttribute":
		var x Vpc
		if !a.db.get("vpc", v.Get("VpcId"), &x) {
			return "", ec2Err("InvalidVpcID.NotFound", "The vpc ID '%s' does not exist", v.Get("VpcId"))
		}
		if s := v.Get("EnableDnsSupport.Value"); s != "" {
			x.DNSSupport = s == "true"
		}
		if s := v.Get("EnableDnsHostnames.Value"); s != "" {
			x.DNSHostnames = s == "true"
		}
		if s := v.Get("EnableNetworkAddressUsageMetrics.Value"); s != "" {
			x.NAUMetrics = s == "true"
		}
		_ = a.db.put("vpc", x.ID, x)
		return ok, nil
	case "DeleteVpc":
		id := v.Get("VpcId")
		if !a.db.has("vpc", id) {
			return "", ec2Err("InvalidVpcID.NotFound", "The vpc ID '%s' does not exist", id)
		}
		for _, s := range list[EC2Subnet](a.db, "subnet") {
			if s.VpcID == id {
				return "", ec2Err("DependencyViolation", "The vpc '%s' has dependencies and cannot be deleted.", id)
			}
		}
		for _, g := range list[IGW](a.db, "igw") {
			if g.VpcID == id {
				return "", ec2Err("DependencyViolation", "The vpc '%s' has dependencies and cannot be deleted.", id)
			}
		}
		for _, g := range list[SecGroup](a.db, "sg") {
			if g.VpcID == id && g.Name != "default" {
				return "", ec2Err("DependencyViolation", "The vpc '%s' has dependencies and cannot be deleted.", id)
			}
		}
		for _, rt := range list[RouteTable](a.db, "routetable") {
			if rt.VpcID != id {
				continue
			}
			main := false
			for _, as := range rt.Assocs {
				main = main || as.Main
			}
			if !main {
				return "", ec2Err("DependencyViolation", "The vpc '%s' has dependencies and cannot be deleted.", id)
			}
			_ = a.db.del("routetable", rt.ID)
			a.untagAll(rt.ID)
		}
		for _, g := range list[SecGroup](a.db, "sg") {
			if g.VpcID == id {
				_ = a.db.del("sg", g.ID)
				a.untagAll(g.ID)
			}
		}
		_ = a.db.del("vpc", id)
		a.untagAll(id)
		return ok, nil

	// ── subnets
	case "CreateSubnet":
		var x Vpc
		if !a.db.get("vpc", v.Get("VpcId"), &x) {
			return "", ec2Err("InvalidVpcID.NotFound", "The vpc ID '%s' does not exist", v.Get("VpcId"))
		}
		s := EC2Subnet{ID: ec2ID("subnet"), VpcID: x.ID, Cidr: v.Get("CidrBlock"), AZ: v.Get("AvailabilityZone"), HostnameType: "ip-name", Created: nowMs()}
		if s.AZ == "" {
			s.AZ = a.cfg.Region + "a"
		}
		for _, cur := range list[EC2Subnet](a.db, "subnet") {
			if cur.VpcID == s.VpcID && cur.Cidr == s.Cidr {
				return "", ec2Err("InvalidSubnet.Conflict", "The CIDR '%s' conflicts with another subnet", s.Cidr)
			}
		}
		_ = a.db.put("subnet", s.ID, s)
		created(s.ID)
		return resp("<subnet>" + a.subnetXML(s) + "</subnet>"), nil
	case "DescribeSubnets":
		ids := listN(v, "SubnetId")
		for _, id := range ids {
			if !a.db.has("subnet", id) {
				return "", ec2Err("InvalidSubnetID.NotFound", "The subnet ID '%s' does not exist", id)
			}
		}
		var sb strings.Builder
		for _, s := range list[EC2Subnet](a.db, "subnet") {
			if (len(ids) > 0 && !contains(ids, s.ID)) || !ec2Match(map[string][]string{"subnet-id": {s.ID}, "vpc-id": {s.VpcID}, "cidr": {s.Cidr},
				"cidr-block": {s.Cidr}, "cidrBlock": {s.Cidr}, "availability-zone": {s.AZ}, "availabilityZone": {s.AZ}, "availability-zone-id": {a.azID(s.AZ)},
				"state": {"available"}, "default-for-az": {"false"}, "defaultForAz": {"false"}, "map-public-ip-on-launch": {boolS(s.MapPublicIP)},
				"owner-id": {a.cfg.Account}, "subnet-arn": {a.arn("ec2", "subnet/"+s.ID)}}, a.tagsOf(s.ID), filters) {
				continue
			}
			sb.WriteString("<item>" + a.subnetXML(s) + "</item>")
		}
		return resp("<subnetSet>" + sb.String() + "</subnetSet>"), nil
	case "ModifySubnetAttribute":
		var s EC2Subnet
		if !a.db.get("subnet", v.Get("SubnetId"), &s) {
			return "", ec2Err("InvalidSubnetID.NotFound", "The subnet ID '%s' does not exist", v.Get("SubnetId"))
		}
		if x := v.Get("MapPublicIpOnLaunch.Value"); x != "" {
			s.MapPublicIP = x == "true"
		}
		if x := v.Get("PrivateDnsHostnameTypeOnLaunch"); x != "" {
			s.HostnameType = x
		}
		if x := v.Get("EnableResourceNameDnsARecordOnLaunch.Value"); x != "" {
			s.DNSA = x == "true"
		}
		if x := v.Get("EnableResourceNameDnsAAAARecordOnLaunch.Value"); x != "" {
			s.DNSAAAA = x == "true"
		}
		_ = a.db.put("subnet", s.ID, s)
		return ok, nil
	case "DeleteSubnet":
		id := v.Get("SubnetId")
		if !a.db.has("subnet", id) {
			return "", ec2Err("InvalidSubnetID.NotFound", "The subnet ID '%s' does not exist", id)
		}
		for _, n := range list[NatGW](a.db, "natgw") {
			if n.SubnetID == id && n.State != "deleted" {
				return "", ec2Err("DependencyViolation", "The subnet '%s' has dependencies and cannot be deleted.", id)
			}
		}
		for _, rt := range list[RouteTable](a.db, "routetable") {
			kept := rt.Assocs[:0]
			for _, as := range rt.Assocs {
				if as.Subnet != id {
					kept = append(kept, as)
				}
			}
			if len(kept) != len(rt.Assocs) {
				rt.Assocs = kept
				_ = a.db.put("routetable", rt.ID, rt)
			}
		}
		_ = a.db.del("subnet", id)
		a.untagAll(id)
		return ok, nil

	// ── security groups
	case "CreateSecurityGroup":
		name, vpc := v.Get("GroupName"), v.Get("VpcId")
		if vpc != "" && !a.db.has("vpc", vpc) {
			return "", ec2Err("InvalidVpcID.NotFound", "The vpc ID '%s' does not exist", vpc)
		}
		for _, g := range list[SecGroup](a.db, "sg") {
			if g.Name == name && g.VpcID == vpc {
				return "", ec2Err("InvalidGroup.Duplicate", "The security group '%s' already exists for VPC '%s'", name, vpc)
			}
		}
		g := SecGroup{ID: ec2ID("sg"), Name: name, Desc: v.Get("GroupDescription"), VpcID: vpc,
			Rules: []SGRule{{ID: ec2ID("sgr"), Egress: true, Protocol: "-1", From: -1, To: -1, Cidr: "0.0.0.0/0"}}}
		_ = a.db.put("sg", g.ID, g)
		created(g.ID)
		return resp("<return>true</return><groupId>" + g.ID + "</groupId><securityGroupArn>" + a.arn("ec2", "security-group/"+g.ID) + "</securityGroupArn>" + tagSet(a.tagsOf(g.ID))), nil
	case "DescribeSecurityGroups":
		ids, names := listN(v, "GroupId"), listN(v, "GroupName")
		for _, id := range ids {
			if !a.db.has("sg", id) {
				return "", ec2Err("InvalidGroup.NotFound", "The security group '%s' does not exist", id)
			}
		}
		var sb strings.Builder
		found := map[string]bool{}
		for _, g := range list[SecGroup](a.db, "sg") {
			if (len(ids) > 0 && !contains(ids, g.ID)) || (len(names) > 0 && !contains(names, g.Name)) ||
				!ec2Match(map[string][]string{"group-id": {g.ID}, "group-name": {g.Name}, "vpc-id": {g.VpcID}, "description": {g.Desc},
					"owner-id": {a.cfg.Account}}, a.tagsOf(g.ID), filters) {
				continue
			}
			found[g.Name] = true
			sb.WriteString("<item>" + a.sgXML(g) + "</item>")
		}
		for _, n := range names {
			if !found[n] {
				return "", ec2Err("InvalidGroup.NotFound", "The security group '%s' does not exist", n)
			}
		}
		return resp("<securityGroupInfo>" + sb.String() + "</securityGroupInfo>"), nil
	case "DeleteSecurityGroup":
		id := v.Get("GroupId")
		var g SecGroup
		if !a.db.get("sg", id, &g) {
			return "", ec2Err("InvalidGroup.NotFound", "The security group '%s' does not exist", id)
		}
		for _, o := range list[SecGroup](a.db, "sg") {
			for _, r := range o.Rules {
				if o.ID != id && r.Group == id {
					return "", ec2Err("DependencyViolation", "resource %s has a dependent object", id)
				}
			}
		}
		_ = a.db.del("sg", id)
		a.untagAll(id)
		return ok, nil
	case "AuthorizeSecurityGroupIngress", "AuthorizeSecurityGroupEgress":
		var g SecGroup
		if !a.db.get("sg", v.Get("GroupId"), &g) {
			return "", ec2Err("InvalidGroup.NotFound", "The security group '%s' does not exist", v.Get("GroupId"))
		}
		rules := parsePermissions(v, action == "AuthorizeSecurityGroupEgress")
		var sb strings.Builder
		for _, r := range rules {
			for _, cur := range g.Rules {
				if cur.sameAs(r) {
					return "", ec2Err("InvalidPermission.Duplicate", "the specified rule \"peer: %s, %s, ALLOW\" already exists", r.peer(), r.Protocol)
				}
			}
		}
		tags := tagSpecs(v)
		for _, r := range rules {
			r.ID = ec2ID("sgr")
			g.Rules = append(g.Rules, r)
			a.tagResource(r.ID, tags)
			sb.WriteString("<item>" + a.ruleXML(g, r) + "</item>")
		}
		_ = a.db.put("sg", g.ID, g)
		return resp("<return>true</return><securityGroupRuleSet>" + sb.String() + "</securityGroupRuleSet>"), nil
	case "RevokeSecurityGroupIngress", "RevokeSecurityGroupEgress":
		var g SecGroup
		if !a.db.get("sg", v.Get("GroupId"), &g) {
			return "", ec2Err("InvalidGroup.NotFound", "The security group '%s' does not exist", v.Get("GroupId"))
		}
		egress := action == "RevokeSecurityGroupEgress"
		drop := map[string]bool{}
		for _, id := range listN(v, "SecurityGroupRuleId") {
			drop[id] = true
		}
		for _, r := range parsePermissions(v, egress) {
			hit := false
			for _, cur := range g.Rules {
				if cur.sameAs(r) {
					drop[cur.ID], hit = true, true
				}
			}
			if !hit {
				return "", ec2Err("InvalidPermission.NotFound", "The specified rule does not exist in this security group.")
			}
		}
		kept := g.Rules[:0]
		for _, r := range g.Rules {
			if drop[r.ID] && r.Egress == egress {
				a.untagAll(r.ID)
				continue
			}
			kept = append(kept, r)
		}
		g.Rules = kept
		_ = a.db.put("sg", g.ID, g)
		return ok, nil
	case "UpdateSecurityGroupRuleDescriptionsIngress", "UpdateSecurityGroupRuleDescriptionsEgress":
		var g SecGroup
		if !a.db.get("sg", v.Get("GroupId"), &g) {
			return "", ec2Err("InvalidGroup.NotFound", "The security group '%s' does not exist", v.Get("GroupId"))
		}
		for _, r := range parsePermissions(v, action == "UpdateSecurityGroupRuleDescriptionsEgress") {
			for i := range g.Rules {
				if g.Rules[i].sameAs(r) {
					g.Rules[i].Desc = r.Desc
				}
			}
		}
		_ = a.db.put("sg", g.ID, g)
		return ok, nil
	case "ModifySecurityGroupRules":
		var g SecGroup
		if !a.db.get("sg", v.Get("GroupId"), &g) {
			return "", ec2Err("InvalidGroup.NotFound", "The security group '%s' does not exist", v.Get("GroupId"))
		}
		for i := 1; ; i++ {
			p := "SecurityGroupRule." + strconv.Itoa(i) + "."
			id := v.Get(p + "SecurityGroupRuleId")
			if id == "" {
				break
			}
			for j := range g.Rules {
				if g.Rules[j].ID != id {
					continue
				}
				r := &g.Rules[j]
				r.Protocol = normProto(v.Get(p + "SecurityGroupRule.IpProtocol"))
				r.From, r.To = atoi(v.Get(p+"SecurityGroupRule.FromPort")), atoi(v.Get(p+"SecurityGroupRule.ToPort"))
				r.Cidr, r.Cidr6 = v.Get(p+"SecurityGroupRule.CidrIpv4"), v.Get(p+"SecurityGroupRule.CidrIpv6")
				r.Group, r.PrefixList = v.Get(p+"SecurityGroupRule.ReferencedGroupId"), v.Get(p+"SecurityGroupRule.PrefixListId")
				r.Desc = v.Get(p + "SecurityGroupRule.Description")
			}
		}
		_ = a.db.put("sg", g.ID, g)
		return ok, nil
	case "DescribeSecurityGroupRules":
		ids := listN(v, "SecurityGroupRuleId")
		var sb strings.Builder
		n := 0
		for _, g := range list[SecGroup](a.db, "sg") {
			for _, r := range g.Rules {
				if (len(ids) > 0 && !contains(ids, r.ID)) || !ec2Match(map[string][]string{"group-id": {g.ID}, "security-group-rule-id": {r.ID}},
					a.tagsOf(r.ID), filters) {
					continue
				}
				n++
				sb.WriteString("<item>" + a.ruleXML(g, r) + "</item>")
			}
		}
		if len(ids) > 0 && n == 0 {
			return "", ec2Err("InvalidSecurityGroupRuleId.NotFound", "The security group rule ID '%s' does not exist", ids[0])
		}
		return resp("<securityGroupRuleSet>" + sb.String() + "</securityGroupRuleSet>"), nil

	// ── route tables
	case "CreateRouteTable":
		var x Vpc
		if !a.db.get("vpc", v.Get("VpcId"), &x) {
			return "", ec2Err("InvalidVpcID.NotFound", "The vpc ID '%s' does not exist", v.Get("VpcId"))
		}
		rt := RouteTable{ID: ec2ID("rtb"), VpcID: x.ID, Routes: []Route{{Dest: x.Cidr, Gateway: "local", Origin: "CreateRouteTable"}}}
		_ = a.db.put("routetable", rt.ID, rt)
		created(rt.ID)
		return resp("<routeTable>" + a.rtXML(rt) + "</routeTable>"), nil
	case "DescribeRouteTables":
		ids := listN(v, "RouteTableId")
		for _, id := range ids {
			if !a.db.has("routetable", id) {
				return "", ec2Err("InvalidRouteTableID.NotFound", "The routeTable ID '%s' does not exist", id)
			}
		}
		var sb strings.Builder
		for _, rt := range list[RouteTable](a.db, "routetable") {
			f := map[string][]string{"route-table-id": {rt.ID}, "vpc-id": {rt.VpcID}, "owner-id": {a.cfg.Account}, "association.main": {"false"}}
			for _, as := range rt.Assocs {
				f["association.route-table-association-id"] = append(f["association.route-table-association-id"], as.ID)
				if as.Main {
					f["association.main"] = []string{"true"}
				}
				if as.Subnet != "" {
					f["association.subnet-id"] = append(f["association.subnet-id"], as.Subnet)
				}
				if as.Gateway != "" {
					f["association.gateway-id"] = append(f["association.gateway-id"], as.Gateway)
				}
			}
			if (len(ids) > 0 && !contains(ids, rt.ID)) || !ec2Match(f, a.tagsOf(rt.ID), filters) {
				continue
			}
			sb.WriteString("<item>" + a.rtXML(rt) + "</item>")
		}
		return resp("<routeTableSet>" + sb.String() + "</routeTableSet>"), nil
	case "DeleteRouteTable":
		id := v.Get("RouteTableId")
		var rt RouteTable
		if !a.db.get("routetable", id, &rt) {
			return "", ec2Err("InvalidRouteTableID.NotFound", "The routeTable ID '%s' does not exist", id)
		}
		if len(rt.Assocs) > 0 {
			return "", ec2Err("DependencyViolation", "The routeTable '%s' has dependencies and cannot be deleted.", id)
		}
		_ = a.db.del("routetable", id)
		a.untagAll(id)
		return ok, nil
	case "CreateRoute", "ReplaceRoute":
		var rt RouteTable
		if !a.db.get("routetable", v.Get("RouteTableId"), &rt) {
			return "", ec2Err("InvalidRouteTableID.NotFound", "The routeTable ID '%s' does not exist", v.Get("RouteTableId"))
		}
		r := Route{Dest: v.Get("DestinationCidrBlock"), Dest6: v.Get("DestinationIpv6CidrBlock"), PrefixList: v.Get("DestinationPrefixListId"),
			Gateway: v.Get("GatewayId"), NatGateway: v.Get("NatGatewayId"), Endpoint: v.Get("VpcEndpointId"), Eni: v.Get("NetworkInterfaceId"),
			TransitGateway: v.Get("TransitGatewayId"), Peering: v.Get("VpcPeeringConnectionId"), Origin: "CreateRoute"}
		replaced := false
		for i, cur := range rt.Routes {
			if cur.Dest == r.Dest && cur.Dest6 == r.Dest6 && cur.PrefixList == r.PrefixList {
				if action == "CreateRoute" {
					return "", ec2Err("RouteAlreadyExists", "The route identified by %s already exists.", r.Dest+r.Dest6+r.PrefixList)
				}
				rt.Routes[i], replaced = r, true
			}
		}
		if action == "ReplaceRoute" && !replaced {
			return "", ec2Err("InvalidRoute.NotFound", "no route with destination-cidr-block %s in route table %s", r.Dest, rt.ID)
		}
		if !replaced {
			rt.Routes = append(rt.Routes, r)
		}
		_ = a.db.put("routetable", rt.ID, rt)
		return ok, nil
	case "DeleteRoute":
		var rt RouteTable
		if !a.db.get("routetable", v.Get("RouteTableId"), &rt) {
			return "", ec2Err("InvalidRouteTableID.NotFound", "The routeTable ID '%s' does not exist", v.Get("RouteTableId"))
		}
		kept := rt.Routes[:0]
		hit := false
		for _, r := range rt.Routes {
			if r.Dest == v.Get("DestinationCidrBlock") && r.Dest6 == v.Get("DestinationIpv6CidrBlock") && r.PrefixList == v.Get("DestinationPrefixListId") {
				hit = true
				continue
			}
			kept = append(kept, r)
		}
		if !hit {
			return "", ec2Err("InvalidRoute.NotFound", "no route with destination-cidr-block %s in route table %s", v.Get("DestinationCidrBlock"), rt.ID)
		}
		rt.Routes = kept
		_ = a.db.put("routetable", rt.ID, rt)
		return ok, nil
	case "AssociateRouteTable":
		var rt RouteTable
		if !a.db.get("routetable", v.Get("RouteTableId"), &rt) {
			return "", ec2Err("InvalidRouteTableID.NotFound", "The routeTable ID '%s' does not exist", v.Get("RouteTableId"))
		}
		sub := v.Get("SubnetId")
		for _, o := range list[RouteTable](a.db, "routetable") {
			for _, as := range o.Assocs {
				if sub != "" && as.Subnet == sub {
					return "", ec2Err("Resource.AlreadyAssociated", "the specified association for route table %s conflicts with an existing association", rt.ID)
				}
			}
		}
		as := RTAssoc{ID: ec2ID("rtbassoc"), Subnet: sub, Gateway: v.Get("GatewayId")}
		rt.Assocs = append(rt.Assocs, as)
		_ = a.db.put("routetable", rt.ID, rt)
		return resp("<associationId>" + as.ID + "</associationId><associationState><state>associated</state></associationState>"), nil
	case "DisassociateRouteTable", "ReplaceRouteTableAssociation":
		id := v.Get("AssociationId")
		var moved *RTAssoc
		for _, rt := range list[RouteTable](a.db, "routetable") {
			for i, as := range rt.Assocs {
				if as.ID == id {
					as := as
					moved = &as
					rt.Assocs = append(rt.Assocs[:i], rt.Assocs[i+1:]...)
					_ = a.db.put("routetable", rt.ID, rt)
					break
				}
			}
		}
		if moved == nil {
			return "", ec2Err("InvalidAssociationID.NotFound", "The association ID '%s' does not exist", id)
		}
		if action == "DisassociateRouteTable" {
			return ok, nil
		}
		var rt RouteTable
		if !a.db.get("routetable", v.Get("RouteTableId"), &rt) {
			return "", ec2Err("InvalidRouteTableID.NotFound", "The routeTable ID '%s' does not exist", v.Get("RouteTableId"))
		}
		moved.ID = ec2ID("rtbassoc")
		rt.Assocs = append(rt.Assocs, *moved)
		_ = a.db.put("routetable", rt.ID, rt)
		return resp("<newAssociationId>" + moved.ID + "</newAssociationId><associationState><state>associated</state></associationState>"), nil

	// ── internet gateways
	case "CreateInternetGateway":
		g := IGW{ID: ec2ID("igw")}
		_ = a.db.put("igw", g.ID, g)
		created(g.ID)
		return resp("<internetGateway>" + a.igwXML(g) + "</internetGateway>"), nil
	case "DescribeInternetGateways":
		ids := listN(v, "InternetGatewayId")
		for _, id := range ids {
			if !a.db.has("igw", id) {
				return "", ec2Err("InvalidInternetGatewayID.NotFound", "The internetGateway ID '%s' does not exist", id)
			}
		}
		var sb strings.Builder
		for _, g := range list[IGW](a.db, "igw") {
			if (len(ids) > 0 && !contains(ids, g.ID)) || !ec2Match(map[string][]string{"internet-gateway-id": {g.ID}, "attachment.vpc-id": {g.VpcID},
				"attachment.state": {"available"}, "owner-id": {a.cfg.Account}}, a.tagsOf(g.ID), filters) {
				continue
			}
			sb.WriteString("<item>" + a.igwXML(g) + "</item>")
		}
		return resp("<internetGatewaySet>" + sb.String() + "</internetGatewaySet>"), nil
	case "AttachInternetGateway", "DetachInternetGateway":
		var g IGW
		if !a.db.get("igw", v.Get("InternetGatewayId"), &g) {
			return "", ec2Err("InvalidInternetGatewayID.NotFound", "The internetGateway ID '%s' does not exist", v.Get("InternetGatewayId"))
		}
		if action == "AttachInternetGateway" {
			if !a.db.has("vpc", v.Get("VpcId")) {
				return "", ec2Err("InvalidVpcID.NotFound", "The vpc ID '%s' does not exist", v.Get("VpcId"))
			}
			g.VpcID = v.Get("VpcId")
		} else {
			g.VpcID = ""
		}
		_ = a.db.put("igw", g.ID, g)
		return ok, nil
	case "DeleteInternetGateway":
		var g IGW
		if !a.db.get("igw", v.Get("InternetGatewayId"), &g) {
			return "", ec2Err("InvalidInternetGatewayID.NotFound", "The internetGateway ID '%s' does not exist", v.Get("InternetGatewayId"))
		}
		if g.VpcID != "" {
			return "", ec2Err("DependencyViolation", "The internetGateway '%s' has dependencies and cannot be deleted.", g.ID)
		}
		_ = a.db.del("igw", g.ID)
		a.untagAll(g.ID)
		return ok, nil

	// ── Elastic IPs and NAT gateways
	case "AllocateAddress":
		n := a.db.next("eip")
		e := EIP{ID: ec2ID("eipalloc"), PublicIP: fmt.Sprintf("198.51.100.%d", n%250+1), Domain: "vpc"}
		_ = a.db.put("eip", e.ID, e)
		created(e.ID)
		return resp("<publicIp>" + e.PublicIP + "</publicIp><domain>vpc</domain><allocationId>" + e.ID + "</allocationId><publicIpv4Pool>amazon</publicIpv4Pool><networkBorderGroup>" +
			a.cfg.Region + "</networkBorderGroup>"), nil
	case "DescribeAddresses":
		ids, ips := listN(v, "AllocationId"), listN(v, "PublicIp")
		var sb strings.Builder
		n := 0
		for _, e := range list[EIP](a.db, "eip") {
			if (len(ids) > 0 && !contains(ids, e.ID)) || (len(ips) > 0 && !contains(ips, e.PublicIP)) ||
				!ec2Match(map[string][]string{"allocation-id": {e.ID}, "public-ip": {e.PublicIP}, "domain": {"vpc"}}, a.tagsOf(e.ID), filters) {
				continue
			}
			n++
			sb.WriteString("<item>" + a.eipXML(e) + "</item>")
		}
		if len(ids) > 0 && n == 0 {
			return "", ec2Err("InvalidAllocationID.NotFound", "The allocation ID '%s' does not exist", ids[0])
		}
		return resp("<addressesSet>" + sb.String() + "</addressesSet>"), nil
	case "AssociateAddress", "DisassociateAddress":
		var found *EIP
		for _, e := range list[EIP](a.db, "eip") {
			if e.ID == v.Get("AllocationId") || (v.Get("AssociationId") != "" && e.Association == v.Get("AssociationId")) {
				e := e
				found = &e
			}
		}
		if found == nil {
			if action == "DisassociateAddress" {
				return "", ec2Err("InvalidAssociationID.NotFound", "The association ID '%s' does not exist", v.Get("AssociationId"))
			}
			return "", ec2Err("InvalidAllocationID.NotFound", "The allocation ID '%s' does not exist", v.Get("AllocationId"))
		}
		if action == "DisassociateAddress" {
			found.Association, found.Eni, found.Instance = "", "", ""
			_ = a.db.put("eip", found.ID, *found)
			return ok, nil
		}
		found.Association, found.Instance, found.Eni = "eipassoc-"+randHex(9)[:17], v.Get("InstanceId"), v.Get("NetworkInterfaceId")
		_ = a.db.put("eip", found.ID, *found)
		return resp("<return>true</return><associationId>" + found.Association + "</associationId>"), nil
	case "DescribeAddressesAttribute":
		return resp("<addressSet/>"), nil
	case "ReleaseAddress":
		if !a.db.has("eip", v.Get("AllocationId")) {
			return "", ec2Err("InvalidAllocationID.NotFound", "The allocation ID '%s' does not exist", v.Get("AllocationId"))
		}
		_ = a.db.del("eip", v.Get("AllocationId"))
		a.untagAll(v.Get("AllocationId"))
		return ok, nil
	case "CreateNatGateway":
		var s EC2Subnet
		if !a.db.get("subnet", v.Get("SubnetId"), &s) {
			return "", ec2Err("InvalidSubnetID.NotFound", "The subnet ID '%s' does not exist", v.Get("SubnetId"))
		}
		conn := v.Get("ConnectivityType")
		if conn == "" {
			conn = "public"
		}
		n := NatGW{ID: ec2ID("nat"), SubnetID: s.ID, VpcID: s.VpcID, AllocationID: v.Get("AllocationId"), Connectivity: conn, State: "available",
			PrivateIP: "10.0.0." + strconv.Itoa(int(a.db.next("natip")%250)+4), Created: nowMs()}
		if n.AllocationID != "" {
			var e EIP
			if a.db.get("eip", n.AllocationID, &e) {
				e.Association, e.Eni = "eipassoc-"+randHex(9)[:17], "eni-"+strings.TrimPrefix(n.ID, "nat-")
				_ = a.db.put("eip", e.ID, e)
			}
		}
		_ = a.db.put("natgw", n.ID, n)
		created(n.ID)
		return resp("<natGateway>" + a.natXML(n) + "</natGateway>"), nil
	case "DescribeNatGateways":
		ids := listN(v, "NatGatewayId")
		var sb strings.Builder
		n := 0
		for _, g := range list[NatGW](a.db, "natgw") {
			if (len(ids) > 0 && !contains(ids, g.ID)) || !ec2Match(map[string][]string{"nat-gateway-id": {g.ID}, "subnet-id": {g.SubnetID},
				"vpc-id": {g.VpcID}, "state": {g.State}}, a.tagsOf(g.ID), filters) {
				continue
			}
			n++
			sb.WriteString("<item>" + a.natXML(g) + "</item>")
		}
		if len(ids) > 0 && n == 0 {
			return "", ec2Err("NatGatewayNotFound", "NAT gateway %s was not found", ids[0])
		}
		return resp("<natGatewaySet>" + sb.String() + "</natGatewaySet>"), nil
	case "DeleteNatGateway":
		var g NatGW
		if !a.db.get("natgw", v.Get("NatGatewayId"), &g) {
			return "", ec2Err("NatGatewayNotFound", "NAT gateway %s was not found", v.Get("NatGatewayId"))
		}
		g.State = "deleted"
		_ = a.db.put("natgw", g.ID, g)
		var e EIP
		if a.db.get("eip", g.AllocationID, &e) {
			e.Association, e.Eni = "", ""
			_ = a.db.put("eip", e.ID, e)
		}
		return resp("<natGatewayId>" + g.ID + "</natGatewayId>"), nil

	// ── read-only views
	case "DescribeNetworkAcls":
		var sb strings.Builder
		for _, x := range list[Vpc](a.db, "vpc") {
			id := defaultACL(x.ID)
			var subs []string
			for _, s := range list[EC2Subnet](a.db, "subnet") {
				if s.VpcID == x.ID {
					subs = append(subs, s.ID)
				}
			}
			if ids := listN(v, "NetworkAclId"); (len(ids) > 0 && !contains(ids, id)) ||
				!ec2Match(map[string][]string{"network-acl-id": {id}, "vpc-id": {x.ID}, "default": {"true"}, "association.subnet-id": subs}, nil, filters) {
				continue
			}
			var assoc strings.Builder
			for _, s := range subs {
				assoc.WriteString("<item><networkAclAssociationId>aclassoc-" + strings.TrimPrefix(s, "subnet-") + "</networkAclAssociationId><networkAclId>" +
					id + "</networkAclId><subnetId>" + s + "</subnetId></item>")
			}
			entries := ""
			for _, eg := range []bool{false, true} {
				for _, e := range []struct {
					n      int
					action string
				}{{100, "allow"}, {32767, "deny"}} {
					entries += "<item><ruleNumber>" + strconv.Itoa(e.n) + "</ruleNumber><protocol>-1</protocol><ruleAction>" + e.action +
						"</ruleAction><egress>" + boolS(eg) + "</egress><cidrBlock>0.0.0.0/0</cidrBlock></item>"
				}
			}
			sb.WriteString("<item><networkAclId>" + id + "</networkAclId><vpcId>" + x.ID + "</vpcId><default>true</default><ownerId>" + a.cfg.Account +
				"</ownerId><entrySet>" + entries + "</entrySet><associationSet>" + assoc.String() + "</associationSet><tagSet/></item>")
		}
		return resp("<networkAclSet>" + sb.String() + "</networkAclSet>"), nil
	case "DescribeAvailabilityZones":
		letters := []string{"a", "b", "c"}
		if a.cfg.Region == "ap-northeast-1" {
			letters = []string{"a", "c", "d"} // the zones that account sees
		}
		var sb strings.Builder
		for _, l := range letters {
			z := a.cfg.Region + l
			if names := listN(v, "ZoneName"); (len(names) > 0 && !contains(names, z)) ||
				!ec2Match(map[string][]string{"zone-name": {z}, "zone-id": {a.azID(z)}, "state": {"available"}, "region-name": {a.cfg.Region},
					"opt-in-status": {"opt-in-not-required"}, "zone-type": {"availability-zone"}, "group-name": {a.cfg.Region}}, nil, filters) {
				continue
			}
			sb.WriteString("<item><zoneName>" + z + "</zoneName><zoneId>" + a.azID(z) + "</zoneId><zoneState>available</zoneState><regionName>" + a.cfg.Region +
				"</regionName><groupName>" + a.cfg.Region + "</groupName><networkBorderGroup>" + a.cfg.Region +
				"</networkBorderGroup><optInStatus>opt-in-not-required</optInStatus><zoneType>availability-zone</zoneType><messageSet/></item>")
		}
		return resp("<availabilityZoneInfo>" + sb.String() + "</availabilityZoneInfo>"), nil
	case "DescribeAccountAttributes":
		return resp("<accountAttributeSet><item><attributeName>supported-platforms</attributeName><attributeValueSet><item><attributeValue>VPC</attributeValue></item></attributeValueSet></item>" +
			"<item><attributeName>default-vpc</attributeName><attributeValueSet><item><attributeValue>none</attributeValue></item></attributeValueSet></item></accountAttributeSet>"), nil
	case "DescribeRegions":
		return resp("<regionInfo><item><regionName>" + a.cfg.Region + "</regionName><regionEndpoint>" + xe(a.cfg.PublicURL) +
			"</regionEndpoint><optInStatus>opt-in-not-required</optInStatus></item></regionInfo>"), nil
	case "DescribeInstances":
		return resp("<reservationSet/>"), nil
	case "DescribeNetworkInterfaces":
		return resp("<networkInterfaceSet/>"), nil
	case "DescribeVpcEndpoints":
		return resp("<vpcEndpointSet/>"), nil
	case "DescribePrefixLists", "DescribeManagedPrefixLists":
		return resp("<prefixListSet/>"), nil

	// ── tags
	case "CreateTags", "DeleteTags":
		tags := map[string]string{}
		var keys []string
		for i := 1; ; i++ {
			k, okk := v["Tag."+strconv.Itoa(i)+".Key"]
			if !okk {
				break
			}
			tags[k[0]] = v.Get("Tag." + strconv.Itoa(i) + ".Value")
			keys = append(keys, k[0])
		}
		for _, id := range listN(v, "ResourceId") {
			if action == "CreateTags" {
				a.tagResource(id, tags)
			} else {
				a.untagResource(id, keys)
			}
		}
		return ok, nil
	case "DescribeTags":
		type row struct{ id, typ, k, val string }
		var rows []row
		kinds := map[string]string{"vpc": "vpc", "subnet": "subnet", "sg": "security-group", "routetable": "route-table", "igw": "internet-gateway",
			"natgw": "natgateway", "eip": "elastic-ip"}
		for kind, typ := range kinds {
			for _, id := range a.ids(kind) {
				for k, val := range a.tagsOf(id) {
					if ec2Match(map[string][]string{"resource-id": {id}, "resource-type": {typ}, "key": {k}, "value": {val}}, nil, filters) {
						rows = append(rows, row{id, typ, k, val})
					}
				}
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].id+rows[i].k < rows[j].id+rows[j].k })
		var sb strings.Builder
		for _, r := range rows {
			sb.WriteString("<item><resourceId>" + r.id + "</resourceId><resourceType>" + r.typ + "</resourceType><key>" + xe(r.k) + "</key><value>" + xe(r.val) + "</value></item>")
		}
		return resp("<tagSet>" + sb.String() + "</tagSet>"), nil
	}
	return "", errf(400, "InvalidAction", "localaws ec2: action %s is not implemented", action)
}

// ids lists the ids of one resource kind.
func (a *App) ids(kind string) []string {
	rows, err := a.db.db.Query(`SELECT id FROM resources WHERE kind=? ORDER BY seq`, kind)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func normProto(p string) string {
	switch strings.ToLower(p) {
	case "", "all", "-1":
		return "-1"
	case "6":
		return "tcp"
	case "17":
		return "udp"
	case "1":
		return "icmp"
	}
	return strings.ToLower(p)
}

// parsePermissions reads IpPermissions.N.* into one rule per source.
func parsePermissions(v url.Values, egress bool) []SGRule {
	var out []SGRule
	for i := 1; ; i++ {
		p := "IpPermissions." + strconv.Itoa(i) + "."
		proto, ok := v[p+"IpProtocol"]
		if !ok {
			return out
		}
		base := SGRule{Egress: egress, Protocol: normProto(proto[0]), From: atoi(v.Get(p + "FromPort")), To: atoi(v.Get(p + "ToPort"))}
		if base.Protocol == "-1" {
			base.From, base.To = -1, -1
		}
		for j := 1; ; j++ {
			q := p + "IpRanges." + strconv.Itoa(j) + "."
			c, ok := v[q+"CidrIp"]
			if !ok {
				break
			}
			r := base
			r.Cidr, r.Desc = c[0], v.Get(q+"Description")
			out = append(out, r)
		}
		for j := 1; ; j++ {
			q := p + "Ipv6Ranges." + strconv.Itoa(j) + "."
			c, ok := v[q+"CidrIpv6"]
			if !ok {
				break
			}
			r := base
			r.Cidr6, r.Desc = c[0], v.Get(q+"Description")
			out = append(out, r)
		}
		for j := 1; ; j++ {
			q := p + "Groups." + strconv.Itoa(j) + "."
			g, ok := v[q+"GroupId"]
			if !ok {
				break
			}
			r := base
			r.Group, r.Desc = g[0], v.Get(q+"Description")
			out = append(out, r)
		}
		for j := 1; ; j++ {
			q := p + "PrefixListIds." + strconv.Itoa(j) + "."
			pl, ok := v[q+"PrefixListId"]
			if !ok {
				break
			}
			r := base
			r.PrefixList, r.Desc = pl[0], v.Get(q+"Description")
			out = append(out, r)
		}
	}
}

func (r SGRule) sameAs(o SGRule) bool {
	return r.Egress == o.Egress && r.Protocol == o.Protocol && (r.Protocol == "-1" || (r.From == o.From && r.To == o.To)) &&
		r.Cidr == o.Cidr && r.Cidr6 == o.Cidr6 && r.Group == o.Group && r.PrefixList == o.PrefixList
}

func (r SGRule) peer() string { return r.Cidr + r.Cidr6 + r.Group + r.PrefixList }
