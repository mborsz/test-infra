/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gcsupload

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/option"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pod-utils/downwardapi"
	"k8s.io/test-infra/prow/pod-utils/gcs"
)

// Run will upload files to GCS as prescribed by
// the options. Any extra files can be passed as
// a parameter and will have the prefix prepended
// to their destination in GCS, so the caller can
// operate relative to the base of the GCS dir.
func (o Options) Run(extra map[string]gcs.UploadFunc) error {
	var builder gcs.RepoPathBuilder
	switch o.PathStrategy {
	case kube.PathStrategyExplicit:
		builder = gcs.NewExplicitRepoPathBuilder()
	case kube.PathStrategyLegacy:
		builder = gcs.NewLegacyRepoPathBuilder(o.DefaultOrg, o.DefaultRepo)
	case kube.PathStrategySingle:
		builder = gcs.NewSingleDefaultRepoPathBuilder(o.DefaultOrg, o.DefaultRepo)
	}

	spec, err := downwardapi.ResolveSpecFromEnv() // TODO: pass in all the config instead of needing this?
	if err != nil {
		return fmt.Errorf("could not resolve job spec: %v", err)
	}

	var gcsPath string
	jobBasePath := gcs.PathForSpec(spec, builder)
	if o.PathPrefix != "" {
		jobBasePath = path.Join(o.PathPrefix, jobBasePath)
	}
	if o.SubDir == "" {
		gcsPath = jobBasePath
	} else {
		gcsPath = path.Join(jobBasePath, o.SubDir)
	}

	uploadTargets := map[string]gcs.UploadFunc{}

	// ensure that an alias exists for any
	// job we're uploading artifacts for
	if alias := gcs.AliasForSpec(spec); alias != "" {
		fullBasePath := "gs://" + path.Join(o.Bucket, jobBasePath)
		uploadTargets[alias] = gcs.DataUpload(strings.NewReader(fullBasePath))
	}

	if latestBuilds := gcs.LatestBuildForSpec(spec, builder); len(latestBuilds) > 0 {
		for _, latestBuild := range latestBuilds {
			uploadTargets[latestBuild] = gcs.DataUpload(strings.NewReader(spec.BuildId))
		}
	}

	for _, item := range o.Items {
		info, err := os.Stat(item)
		if err != nil {
			logrus.Warnf("Encountered error in resolving items to upload for %s: %v", item, err)
			continue
		}
		if info.IsDir() {
			gatherArtifacts(item, gcsPath, info.Name(), uploadTargets)
		} else {
			destination := path.Join(gcsPath, info.Name())
			if _, exists := uploadTargets[destination]; exists {
				logrus.Warnf("Encountered duplicate upload of %s, skipping...", destination)
				continue
			}
			uploadTargets[destination] = gcs.FileUpload(item)
		}
	}

	for destination, upload := range extra {
		uploadTargets[path.Join(gcsPath, destination)] = upload
	}

	if !o.DryRun {
		ctx := context.Background()
		gcsClient, err := storage.NewClient(ctx, option.WithCredentialsFile(o.GcsCredentialsFile))
		if err != nil {
			return fmt.Errorf("could not connect to GCS: %v", err)
		}

		if err := gcs.Upload(gcsClient.Bucket(o.Bucket), uploadTargets); err != nil {
			return fmt.Errorf("failed to upload to GCS: %v", err)
		}
	} else {
		for destination := range uploadTargets {
			logrus.WithField("dest", destination).Info("Would upload")
		}
	}

	logrus.Info("Finished upload to GCS")
	return nil
}

func gatherArtifacts(artifactDir, gcsPath, subDir string, uploadTargets map[string]gcs.UploadFunc) {
	logrus.Printf("Gathering artifacts from artifact directory: %s", artifactDir)
	filepath.Walk(artifactDir, func(fspath string, info os.FileInfo, err error) error {
		if info == nil || info.IsDir() {
			return nil
		}

		// we know path will be below artifactDir, but we can't
		// communicate that to the filepath module. We can ignore
		// this error as we can be certain it won't occur and best-
		// effort upload is OK in any case
		if relPath, err := filepath.Rel(artifactDir, fspath); err == nil {
			destination := path.Join(subDir, relPath)
			if _, exists := uploadTargets[destination]; exists {
				logrus.Warnf("Encountered duplicate upload of %s, skipping...", destination)
				return nil
			}
			logrus.Printf("Found %s in artifact directory. Uploading as %s\n", fspath, destination)
			uploadTargets[path.Join(gcsPath, destination)] = gcs.FileUpload(fspath)
		} else {
			logrus.Warnf("Encountered error in relative path calculation for %s under %s: %v", fspath, artifactDir, err)
		}
		return nil
	})
}
