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
// gateway image built with the policy tree under test. The framework supplies the step
// definitions, the canonical resource templates and the cleanup deleters; steps that only these
// policies need live in the steps package.
package it_test

import (
	"testing"

	"github.com/cucumber/godog"

	frameworkruntime "github.com/wso2/api-platform/tests/framework/core/runtime"
	"github.com/wso2/api-platform/tests/framework/core/suiterun"
	frameworksteps "github.com/wso2/api-platform/tests/framework/suites/it/steps"
	"github.com/wso2/api-platform/tests/framework/suites/it/steps/platformgateway"

	"github.com/wso2/gateway-controllers/tests/it/steps"
)

// options describes the policy integration suite to the shared suite runner.
func options() suiterun.Options {
	return suiterun.Options{
		Steps: func(topo *frameworkruntime.Topology, featureRoot string) (frameworkruntime.StepRegistrar, error) {
			suite, err := frameworksteps.New(topo, featureRoot)
			if err != nil {
				return nil, err
			}
			return func(sc *godog.ScenarioContext) {
				suite.Register(sc)
				steps.Register(sc, topo)
			}, nil
		},
		CleanupDeleters: platformgateway.RegisterDeleters,
	}
}

func TestMain(m *testing.M) {
	suiterun.Main(m, options())
}

// TestIntegrationSuite runs the policy integration suite declared in it-suite.yaml.
func TestIntegrationSuite(t *testing.T) {
	suiterun.Run(t, options())
}
