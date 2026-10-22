package main

// `localaws terraform-override` writes the Terraform override file that points an
// existing root module at localaws without editing it: every `provider "aws"` block
// (and each alias) gets the localaws endpoints and dummy credentials, the backend moves
// to a local file or to localaws's own S3, and terraform_remote_state data sources that
// read S3 follow. Terraform merges files named *_override.tf over the others, so
// deleting the file points the code back at AWS.
//
//	localaws terraform-override -dir infra/envs/e2e > infra/envs/e2e/localaws_override.tf
//	localaws terraform-override -dir . -backend s3 -o localaws_override.tf

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// tfEndpoints: the provider's endpoint names sent to localaws. Services localaws does
// not emulate are listed too, so they fail with a clear "not emulated" error instead of
// reaching real AWS.
var tfEndpoints = []string{"acm", "apigateway", "appautoscaling", "batch", "cloudfront", "cloudwatch", "dynamodb", "ec2", "ecr", "ecs",
	"elasticache", "elbv2", "events", "iam", "kms", "lambda", "logs", "rds", "route53", "s3", "s3control", "scheduler", "secretsmanager",
	"ses", "sesv2", "sfn", "sns", "sqs", "ssm", "sts", "wafv2"}

type tfScan struct {
	aliases      []string
	withRegion   map[string]bool   // provider (alias, "" = default) → sets region itself
	backendS3    map[string]string // bucket, key, region of the module's backend "s3"
	remoteStates map[string]map[string]string
}

var (
	reProvider    = regexp.MustCompile(`(?m)^\s*provider\s+"aws"\s*\{`)
	reAlias       = regexp.MustCompile(`^\s*alias\s*=\s*"([^"]+)"`)
	reRegion      = regexp.MustCompile(`^\s*region\s*=`)
	reBackendS3   = regexp.MustCompile(`(?m)^\s*backend\s+"s3"\s*\{`)
	reRemoteState = regexp.MustCompile(`(?m)^\s*data\s+"terraform_remote_state"\s+"([^"]+)"\s*\{`)
	reStrAttr     = regexp.MustCompile(`(?m)^\s*(bucket|key|region|workspace_key_prefix)\s*=\s*"([^"]*)"`)
	reBackendAttr = regexp.MustCompile(`(?m)^\s*backend\s*=\s*"([^"]+)"`)
)

// block returns the text of the brace-balanced block starting at the '{' in src[from:].
func block(src string, from int) string {
	i := strings.IndexByte(src[from:], '{')
	if i < 0 {
		return ""
	}
	depth := 0
	for j := from + i; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[from+i+1 : j]
			}
		}
	}
	return src[from+i+1:]
}

// topLevel keeps the lines of a block outside its nested blocks.
func topLevel(body string) string {
	var out []string
	depth := 0
	for _, line := range strings.Split(body, "\n") {
		if depth == 0 {
			out = append(out, line)
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
	}
	return strings.Join(out, "\n")
}

func strAttrs(body string) map[string]string {
	m := map[string]string{}
	for _, g := range reStrAttr.FindAllStringSubmatch(body, -1) {
		m[g[1]] = g[2]
	}
	return m
}

func scanModule(dir string) (tfScan, error) {
	sc := tfScan{remoteStates: map[string]map[string]string{}, withRegion: map[string]bool{}}
	files, err := filepath.Glob(filepath.Join(dir, "*.tf"))
	if err != nil {
		return sc, err
	}
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_override.tf") || filepath.Base(f) == "override.tf" {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return sc, err
		}
		src := stripComments(string(raw))
		for _, loc := range reProvider.FindAllStringIndex(src, -1) {
			alias, region := "", false
			for _, line := range strings.Split(topLevel(block(src, loc[0])), "\n") {
				if g := reAlias.FindStringSubmatch(line); g != nil {
					alias = g[1]
				}
				region = region || reRegion.MatchString(line)
			}
			if alias != "" && !seen[alias] {
				seen[alias] = true
				sc.aliases = append(sc.aliases, alias)
			}
			sc.withRegion[alias] = sc.withRegion[alias] || region
		}
		if loc := reBackendS3.FindStringIndex(src); loc != nil {
			sc.backendS3 = strAttrs(block(src, loc[0]))
		}
		for _, loc := range reRemoteState.FindAllStringSubmatchIndex(src, -1) {
			body := block(src, loc[0])
			if g := reBackendAttr.FindStringSubmatch(body); g == nil || g[1] != "s3" {
				continue
			}
			sc.remoteStates[src[loc[2]:loc[3]]] = strAttrs(body)
		}
	}
	sort.Strings(sc.aliases)
	return sc, nil
}

var reComment = regexp.MustCompile(`(?m)^\s*(#|//).*$`)

func stripComments(s string) string { return reComment.ReplaceAllString(s, "") }

func hclStr(s string) string { return fmt.Sprintf("%q", s) }

// terraformOverride is the `localaws terraform-override` command.
func terraformOverride(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("terraform-override", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "the root module to point at localaws")
	endpoint := fs.String("endpoint", envOr("LOCALAWS_ENDPOINT", "http://localhost:4566"), "localaws URL as Terraform reaches it")
	backend := fs.String("backend", "auto", "state: local (a file in -dir), s3 (localaws's S3; creates the bucket), keep, or auto (s3 when the module uses an s3 backend, else keep)")
	out := fs.String("o", "", "write here instead of stdout (e.g. localaws_override.tf)")
	region := fs.String("region", "", "region for provider blocks that set none (default: localaws's, from /_localaws/health)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: localaws terraform-override [-dir DIR] [-endpoint URL] [-backend auto|local|s3|keep] [-o FILE]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sc, err := scanModule(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "localaws:", err)
		return 1
	}
	mode := *backend
	if mode == "auto" {
		mode = "keep"
		if sc.backendS3 != nil {
			mode = "s3"
		}
	}
	ep := strings.TrimRight(*endpoint, "/")
	if *region == "" {
		*region = "us-east-1"
		if resp, err := http.Get(ep + "/_localaws/health"); err == nil {
			var h M
			if json.NewDecoder(resp.Body).Decode(&h) == nil && getStr(h, "region") != "" {
				*region = getStr(h, "region")
			}
			resp.Body.Close()
		}
	}
	var b strings.Builder
	b.WriteString("# Generated by `localaws terraform-override` — points this module at localaws (" + ep + ").\n")
	b.WriteString("# Terraform merges *_override.tf over the other files; delete this file to target AWS again.\n")
	b.WriteString("# localaws has one account and one region: provider aliases for other regions land there too.\n\n")

	buckets := map[string]bool{}
	s3Backend := func(indent string, attrs map[string]string, defKey string) string {
		bucket, key := attrs["bucket"], attrs["key"]
		if bucket == "" {
			bucket = "terraform-state"
		}
		if key == "" {
			key = defKey
		}
		buckets[bucket] = true
		lines := []string{"bucket = " + hclStr(bucket), "key = " + hclStr(key), "region = " + hclStr(orDefault(attrs["region"], "us-east-1")),
			"endpoints = { s3 = " + hclStr(ep) + ", sts = " + hclStr(ep) + ", iam = " + hclStr(ep) + " }",
			"use_path_style = true", "access_key = \"test\"", "secret_key = \"test\"", "skip_credentials_validation = true",
			"skip_region_validation = true", "skip_requesting_account_id = true", "skip_metadata_api_check = true", "skip_s3_checksum = true"}
		if p := attrs["workspace_key_prefix"]; p != "" {
			lines = append(lines, "workspace_key_prefix = "+hclStr(p))
		}
		return indent + strings.Join(lines, "\n"+indent) + "\n"
	}
	switch mode {
	case "local":
		b.WriteString("terraform {\n  backend \"local\" {\n    path = \"terraform.localaws.tfstate\"\n  }\n}\n\n")
	case "s3":
		attrs := sc.backendS3
		if attrs == nil {
			attrs = map[string]string{}
		}
		b.WriteString("terraform {\n  backend \"s3\" {\n" + s3Backend("    ", attrs, filepath.Base(absDir(*dir))+".tfstate") +
			"    use_lockfile = true\n  }\n}\n\n")
		names := sortedKeys(sc.remoteStates)
		for _, n := range names {
			b.WriteString("data \"terraform_remote_state\" " + hclStr(n) + " {\n  backend = \"s3\"\n  config = {\n" +
				s3Backend("    ", sc.remoteStates[n], n+".tfstate") + "  }\n}\n\n")
		}
	case "keep":
	default:
		fmt.Fprintf(stderr, "localaws: -backend must be auto, local, s3 or keep (got %q)\n", mode)
		return 2
	}

	provider := func(alias string) {
		b.WriteString("provider \"aws\" {\n")
		if alias != "" {
			b.WriteString(fmt.Sprintf("  %-27s = %s\n", "alias", hclStr(alias)))
		}
		if !sc.withRegion[alias] {
			b.WriteString(fmt.Sprintf("  %-27s = %s\n", "region", hclStr(*region)))
		}
		b.WriteString(`  access_key                  = "test"
  secret_key                  = "test"
  token                       = null
  profile                     = null
  allowed_account_ids         = null
  forbidden_account_ids       = null
  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
  skip_requesting_account_id  = false
  s3_use_path_style           = true
  # S3 Control prefixes the host with the account id (000000000000.localhost); through
  # localaws as a proxy that name needs no DNS. HTTPS — a service with no endpoint below,
  # on its way to real AWS — is refused by the same proxy.
  http_proxy  = ` + hclStr(ep) + `
  https_proxy = ` + hclStr(ep) + `

  endpoints {
`)
		for _, e := range tfEndpoints {
			b.WriteString(fmt.Sprintf("    %-14s = %s\n", e, hclStr(ep)))
		}
		b.WriteString("  }\n}\n\n")
	}
	provider("")
	for _, al := range sc.aliases {
		provider(al)
	}

	for bucket := range buckets { // the state bucket must exist before terraform init
		req, _ := http.NewRequest(http.MethodPut, ep+"/"+bucket, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintf(stderr, "localaws: cannot create the state bucket %s at %s: %v\n", bucket, ep, err)
			return 1
		}
		resp.Body.Close()
		if resp.StatusCode != 200 && resp.StatusCode != 409 {
			fmt.Fprintf(stderr, "localaws: creating the state bucket %s: HTTP %d\n", bucket, resp.StatusCode)
			return 1
		}
	}

	text := strings.TrimRight(b.String(), "\n") + "\n"
	if *out == "" {
		_, _ = io.WriteString(stdout, text)
		return 0
	}
	path := *out
	if !filepath.IsAbs(path) && filepath.Dir(path) == "." {
		path = filepath.Join(*dir, path)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		fmt.Fprintln(stderr, "localaws:", err)
		return 1
	}
	fmt.Fprintf(stderr, "localaws: wrote %s (providers: default%s; backend: %s)\n", path, prefixed(sc.aliases), mode)
	return 0
}

func prefixed(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return ", " + strings.Join(l, ", ")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func absDir(d string) string {
	if a, err := filepath.Abs(d); err == nil {
		return a
	}
	return d
}
