package main

// ECR (JSON, AmazonEC2ContainerRegistry_V20150921.*): repositories with their settings,
// lifecycle and repository policies, tags, and an authorization token. There is no
// registry behind it — images are never pushed here; runner.docker.images maps ECR URIs
// to local tags instead.

import (
	"encoding/base64"
	"time"
)

type Repository struct {
	Name, Mutability, Lifecycle, Policy string
	ScanOnPush                          bool
	Encryption                          M
	Created                             int64
}

func (a *App) repoArn(name string) string { return a.arn("ecr", "repository/"+name) }

func (a *App) repoURI(name string) string {
	return a.cfg.Account + ".dkr.ecr." + a.cfg.Region + ".amazonaws.com/" + name
}

func (a *App) repoOut(r Repository) M {
	enc := r.Encryption
	if len(enc) == 0 {
		enc = M{"encryptionType": "AES256"}
	}
	return M{"repositoryArn": a.repoArn(r.Name), "registryId": a.cfg.Account, "repositoryName": r.Name, "repositoryUri": a.repoURI(r.Name),
		"createdAt": epoch(r.Created), "imageTagMutability": r.Mutability, "imageScanningConfiguration": M{"scanOnPush": r.ScanOnPush},
		"encryptionConfiguration": enc}
}

func (a *App) ecrAPI(op string, in M) (any, *apiError) {
	notFound := func(name string) *apiError {
		return errf(400, "RepositoryNotFoundException", "The repository with name '%s' does not exist in the registry with id '%s'", name, a.cfg.Account)
	}
	get := func() (Repository, *apiError) {
		var r Repository
		if !a.db.get("ecrrepo", getStr(in, "repositoryName"), &r) {
			return r, notFound(getStr(in, "repositoryName"))
		}
		return r, nil
	}
	switch op {
	case "CreateRepository":
		name := getStr(in, "repositoryName")
		if a.db.has("ecrrepo", name) {
			return nil, errf(400, "RepositoryAlreadyExistsException", "The repository with name '%s' already exists in the registry with id '%s'", name, a.cfg.Account)
		}
		r := Repository{Name: name, Mutability: getStr(in, "imageTagMutability"), ScanOnPush: getBool(getMap(in, "imageScanningConfiguration"), "scanOnPush"),
			Encryption: getMap(in, "encryptionConfiguration"), Created: nowMs()}
		if r.Mutability == "" {
			r.Mutability = "MUTABLE"
		}
		_ = a.db.put("ecrrepo", name, r)
		a.tagResource(a.repoArn(name), tagsFromList(getList(in, "tags"), "Key", "Value"))
		return M{"repository": a.repoOut(r)}, nil
	case "DescribeRepositories":
		names := strList(getList(in, "repositoryNames"))
		out := []any{}
		for _, n := range names {
			var r Repository
			if !a.db.get("ecrrepo", n, &r) {
				return nil, notFound(n)
			}
			out = append(out, a.repoOut(r))
		}
		if len(names) == 0 {
			for _, r := range list[Repository](a.db, "ecrrepo") {
				out = append(out, a.repoOut(r))
			}
		}
		return M{"repositories": out}, nil
	case "DeleteRepository":
		r, e := get()
		if e != nil {
			return nil, e
		}
		_ = a.db.del("ecrrepo", r.Name)
		a.untagAll(a.repoArn(r.Name))
		return M{"repository": a.repoOut(r)}, nil
	case "PutImageTagMutability", "PutImageScanningConfiguration", "PutLifecyclePolicy", "SetRepositoryPolicy",
		"GetLifecyclePolicy", "GetRepositoryPolicy", "DeleteLifecyclePolicy", "DeleteRepositoryPolicy":
		r, e := get()
		if e != nil {
			return nil, e
		}
		base := M{"registryId": a.cfg.Account, "repositoryName": r.Name}
		switch op {
		case "PutImageTagMutability":
			r.Mutability = getStr(in, "imageTagMutability")
			base["imageTagMutability"] = r.Mutability
		case "PutImageScanningConfiguration":
			r.ScanOnPush = getBool(getMap(in, "imageScanningConfiguration"), "scanOnPush")
			base["imageScanningConfiguration"] = M{"scanOnPush": r.ScanOnPush}
		case "PutLifecyclePolicy":
			r.Lifecycle = getStr(in, "lifecyclePolicyText")
			base["lifecyclePolicyText"] = r.Lifecycle
		case "SetRepositoryPolicy":
			r.Policy = getStr(in, "policyText")
			base["policyText"] = r.Policy
		case "GetLifecyclePolicy", "DeleteLifecyclePolicy":
			if r.Lifecycle == "" {
				return nil, errf(400, "LifecyclePolicyNotFoundException", "Lifecycle policy does not exist for the repository with name '%s' in the registry with id '%s'", r.Name, a.cfg.Account)
			}
			base["lifecyclePolicyText"], base["lastEvaluatedAt"] = r.Lifecycle, epoch(r.Created)
			if op == "GetLifecyclePolicy" {
				return base, nil
			}
			r.Lifecycle = ""
		case "GetRepositoryPolicy", "DeleteRepositoryPolicy":
			if r.Policy == "" {
				return nil, errf(400, "RepositoryPolicyNotFoundException", "Repository policy does not exist for the repository with name '%s' in the registry with id '%s'", r.Name, a.cfg.Account)
			}
			base["policyText"] = r.Policy
			if op == "GetRepositoryPolicy" {
				return base, nil
			}
			r.Policy = ""
		}
		_ = a.db.put("ecrrepo", r.Name, r)
		return base, nil
	case "TagResource":
		a.tagResource(getStr(in, "resourceArn"), tagsFromList(getList(in, "tags"), "Key", "Value"))
		return M{}, nil
	case "UntagResource":
		a.untagResource(getStr(in, "resourceArn"), strList(getList(in, "tagKeys")))
		return M{}, nil
	case "ListTagsForResource":
		return M{"tags": tagList(a.tagsOf(getStr(in, "resourceArn")), "Key", "Value")}, nil
	case "GetAuthorizationToken":
		return M{"authorizationData": []any{M{"authorizationToken": base64.StdEncoding.EncodeToString([]byte("AWS:localaws")),
			"expiresAt": epoch(time.Now().Add(12 * time.Hour).UnixMilli()), "proxyEndpoint": "https://" + a.cfg.Account + ".dkr.ecr." + a.cfg.Region + ".amazonaws.com"}}}, nil
	case "DescribeImages":
		if _, e := get(); e != nil {
			return nil, e
		}
		return M{"imageDetails": []any{}}, nil
	case "ListImages":
		if _, e := get(); e != nil {
			return nil, e
		}
		return M{"imageIds": []any{}}, nil
	case "BatchDeleteImage":
		return M{"imageIds": []any{}, "failures": []any{}}, nil
	case "DescribeRegistry":
		return M{"registryId": a.cfg.Account, "replicationConfiguration": M{"rules": []any{}}}, nil
	case "GetRegistryScanningConfiguration":
		return M{"registryId": a.cfg.Account, "scanningConfiguration": M{"scanType": "BASIC", "rules": []any{}}}, nil
	}
	return nil, errf(400, "UnsupportedOperationException", "localaws ecr: %s is not implemented", op)
}
