/*
Copyright 2020 The Knative Authors

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

// Package kubetest2 is DEPRECATED. Please use https://github.com/kubernetes-sigs/kubetest2/tree/master/kubetest2-gke/deployer directly.
package kubetest2

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"knative.dev/test-infra/pkg/cmd"
	"knative.dev/test-infra/pkg/helpers"
	"knative.dev/test-infra/pkg/metautil"
	"knative.dev/test-infra/pkg/prow"
)

const (
	createCommandTmpl = "%s container clusters create --quiet --enable-autoscaling --min-nodes=%d --max-nodes=%d " +
		"--scopes=%s"
	clusterNamePrefix                  = "e2e-cls"
	boskosAcquireDefaultTimeoutSeconds = 1200
)

var (
	baseKubetest2Flags = []string{"gke", "--ignore-gcp-ssh-key=true", "--up", "--down", "-v=1"}

	// If one of the error patterns below is matched, it would be recommended to
	// retry creating the cluster in a different region.
	// - stockout (https://github.com/knative/test-infra/issues/592)
	retryableErrorPatterns = `.*does not have enough resources available to fulfill.*` +
		`,.*only \d+ nodes out of \d+ have registered; this is likely due to Nodes failing to start correctly.*` +
		`,.*All cluster resources were brought up.+ but: component .+ from endpoint .+ is unhealthy.*`
)

// GKEClusterConfig are the supported configurations for creating a GKE cluster.
type GKEClusterConfig struct {
	GCPServiceAccount string
	GCPProjectID      string

	BoskosAcquireTimeoutSeconds int

	Environment  string
	CommandGroup string

	Name                              string
	Region                            string
	BackupRegions                     []string
	Machine                           string
	MinNodes                          int
	MaxNodes                          int
	Network                           string
	ReleaseChannel                    string
	Version                           string
	Scopes                            string
	Addons                            string
	ImageType                         string
	EnableWorkloadIdentity            bool
	PrivateClusterAccessLevel         string
	PrivateClusterMasterIPSubnetRange []string

	ExtraGcloudFlags string
}

// Run will run the `kubetest2 gke` command with the provided parameters,
// it will also handle the logic that is only used for Knative integration testing, like retrying cluster creation.
func Run(opts *Options, cc *GKEClusterConfig) error {
	kubetest2Flags := baseKubetest2Flags

	createCommand := fmt.Sprintf(createCommandTmpl, cc.CommandGroup, cc.MinNodes, cc.MaxNodes, cc.Scopes)
	if cc.ReleaseChannel != "" {
		createCommand += " --release-channel=" + cc.ReleaseChannel
	}
	kubetest2Flags = append(kubetest2Flags, "--version="+cc.Version)
	if cc.Addons != "" {
		createCommand += " --addons=" + cc.Addons
	}
	if cc.ExtraGcloudFlags != "" {
		createCommand += " " + cc.ExtraGcloudFlags
	}
	kubetest2Flags = append(kubetest2Flags, "--create-command="+createCommand)

	// If cluster name is not provided, generate a random name.
	if cc.Name == "" {
		cc.Name = helpers.AppendRandomString(clusterNamePrefix)
	}
	kubetest2Flags = append(kubetest2Flags, "--cluster-name="+cc.Name, "--environment="+cc.Environment,
		"--num-nodes="+strconv.Itoa(cc.MinNodes), "--machine-type="+cc.Machine, "--network="+cc.Network,
		"--image-type="+cc.ImageType)
	if cc.GCPServiceAccount != "" {
		kubetest2Flags = append(kubetest2Flags, "--gcp-service-account="+cc.GCPServiceAccount)
	}
	if cc.EnableWorkloadIdentity {
		kubetest2Flags = append(kubetest2Flags, "--enable-workload-identity")
	}

	if prow.IsCI() && cc.GCPProjectID == "" {
		log.Println("Will use boskos to provision the GCP project")
		timeout := cc.BoskosAcquireTimeoutSeconds
		if timeout == 0 {
			timeout = boskosAcquireDefaultTimeoutSeconds
		}
		kubetest2Flags = append(kubetest2Flags, "--boskos-acquire-timeout-seconds="+strconv.Itoa(timeout))
	} else {
		if cc.GCPProjectID == "" {
			return errors.New("GCP project must be provided in non-CI environment")
		}
		log.Printf("Will use the GCP project %q for creating the cluster", cc.GCPProjectID)
		kubetest2Flags = append(kubetest2Flags, "--project="+cc.GCPProjectID)
	}

	if cc.PrivateClusterAccessLevel != "" {
		kubetest2Flags = append(kubetest2Flags, "--private-cluster-access-level="+cc.PrivateClusterAccessLevel)
		kubetest2Flags = append(kubetest2Flags, "--private-cluster-master-ip-range="+strings.Join(cc.PrivateClusterMasterIPSubnetRange, ","))
	}

	regions := append([]string{cc.Region}, cc.BackupRegions...)
	kubetest2Flags = append(kubetest2Flags, "--region="+strings.Join(regions, ","))
	kubetest2Flags = append(kubetest2Flags, "--retryable-error-patterns='"+retryableErrorPatterns+"'")

	// Test command args must come last.
	if opts.TestCommand != "" {
		kubetest2Flags = append(kubetest2Flags, "--test=exec", "--")
		kubetest2Flags = append(kubetest2Flags, strings.Split(opts.TestCommand, " ")...)
	}

	log.Printf("Running kubetest2 with flags: %q", kubetest2Flags)

	command := exec.Command("kubetest2", kubetest2Flags...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return err
	}

	// Only save the metadata if it's in CI environment and meta data is asked to be saved.
	if prow.IsCI() && opts.SaveMetaData {
		return saveMetaData(cc, strings.Join(regions, ","))
	}
	return nil
}

// saveMetaData will save the metadata with best effort.
func saveMetaData(cc *GKEClusterConfig, region string) error {
	cli, err := metautil.NewClient("")
	if err != nil {
		return fmt.Errorf("error creating the metautil client: %w", err)
	}
	cv, err := cmd.RunCommand("kubectl version --short=true")
	if err != nil {
		return fmt.Errorf("error getting the cluster version: %w", err)
	}

	// Set the metadata with best effort.
	cli.Set("E2E:Provider", "gke")
	cli.Set("E2E:Region", region)
	cli.Set("E2E:Machine", cc.Machine)
	cli.Set("E2E:Version", cv)
	cli.Set("E2E:MinNodes", strconv.Itoa(cc.MinNodes))
	cli.Set("E2E:MaxNodes", strconv.Itoa(cc.MaxNodes))
	return nil
}
