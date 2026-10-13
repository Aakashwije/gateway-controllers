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

// Package steps holds the step definitions of the policy integration suite that the test
// framework does not provide. Every policy's scenarios share it, so a step is written once here
// and reused, and it is added only after the framework's steps and this package were searched.
package steps

import (
	"github.com/cucumber/godog"

	frameworkruntime "github.com/wso2/api-platform/tests/framework/core/runtime"
)

// Register binds the suite's own steps to the scenario. The framework's steps are bound before
// it runs, so a step defined here must not repeat one of theirs.
func Register(_ *godog.ScenarioContext, _ *frameworkruntime.Topology) {}
