/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

// Package it runs the integration tests of the gateway policies in this repository. Each policy
// declares its runners and feature files in policies/<name>/it, and the suite below boots a
// gateway image built with the policy tree under test.
package it_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/wso2/api-platform/tests/framework/core/cleanup"
	frameworkruntime "github.com/wso2/api-platform/tests/framework/core/runtime"
	"github.com/wso2/api-platform/tests/framework/core/suiterun"
	"github.com/wso2/api-platform/tests/framework/core/util/httpx"

	"github.com/wso2/gateway-controllers/tests/it/steps"
	"github.com/wso2/gateway-controllers/tests/it/steps/platformgateway"
)

// options describes the policy integration suite to the shared suite runner.
func options() suiterun.Options {
	return suiterun.Options{
		Steps: func(topo *frameworkruntime.Topology, featureRoot string) (frameworkruntime.StepRegistrar, error) {
			suite, err := steps.New(topo, featureRoot)
			if err != nil {
				return nil, err
			}
			return suite.Register, nil
		},
		CleanupDeleters: registerDeleters,
	}
}

func TestMain(m *testing.M) {
	suiterun.Main(m, options())
}

// TestIntegrationSuite runs the policy integration suite declared in it-suite.yaml.
func TestIntegrationSuite(t *testing.T) {
	suiterun.Run(t, options())
}

// registerDeleters configures cleanup handlers for gateway resources.
func registerDeleters(reg *cleanup.Registry, topo *frameworkruntime.Topology) {
	client := httpx.NewClient(httpx.Options{MaxRetries: 1, InsecureSkipVerify: true})

	reg.RegisterDeleter(cleanup.KindAPI, func(ctx context.Context, res cleanup.Resource) error {
		return deleteResource(ctx, topo, client, "/rest-apis", res)
	})

	for kind, collection := range map[cleanup.Kind]string{
		cleanup.KindLLMProvider:         "/llm-providers",
		cleanup.KindLLMProxy:            "/llm-proxies",
		cleanup.KindLLMProviderTemplate: "/llm-provider-templates",
		cleanup.KindMCPProxy:            "/mcp-proxies",
		cleanup.KindAgent:               "/agents",
		cleanup.KindCertificate:         "/certificates",
		cleanup.KindSecret:              "/secrets",
	} {
		reg.RegisterDeleter(kind, func(ctx context.Context, res cleanup.Resource) error {
			return deleteResource(ctx, topo, client, collection, res)
		})
	}
}

// deleteResource removes one resource from the gateway controller's management API. A 404 means
// it is already gone, which is success for a sweep; anything else is a real leak signal.
func deleteResource(
	ctx context.Context,
	topo *frameworkruntime.Topology,
	client *httpx.Client,
	collection string,
	res cleanup.Resource,
) error {
	base, err := topo.URL("platform-gateway", "rest")
	if err != nil {
		return err
	}
	version, err := topo.ComponentVersion("platform-gateway")
	if err != nil {
		return err
	}
	resp, err := client.Do(ctx, httpx.Request{
		Method: http.MethodDelete,
		URL:    base + platformgateway.ManagementBasePathForVersion(version) + collection + "/" + res.ID,
		Headers: map[string]string{
			"Authorization": steps.BasicAuthHeader(topo.Admin.Username, topo.Admin.Password),
		},
	}, 1, 0)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound || resp.Succeeded() {
		return nil
	}
	return &deleteError{resp: resp}
}

type deleteError struct{ resp *httpx.Response }

func (e *deleteError) Error() string { return e.resp.Describe() }
