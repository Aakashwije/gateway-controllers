# --------------------------------------------------------------------
# Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
#
# WSO2 LLC. licenses this file to you under the Apache License,
# Version 2.0 (the "License"); you may not use this file except
# in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
# --------------------------------------------------------------------

.PHONY: generate-catalog validate-catalog it it-unit it-standards it-scaffold

generate-catalog: ## Generate docs/README.md policy catalog from metadata.json files
	python3 scripts/generate-policy-catalog.py

validate-catalog: ## Verify docs/README.md is up to date (used in CI)
	python3 scripts/generate-policy-catalog.py --validate

# Policy integration tests. They need Docker and run against a released gateway image:
#   make it POLICIES=time-based-model-routing GATEWAY_VERSION=1.2.0
# POLICIES defaults to every policy that has tests. DB limits the run to one database
# (sqlite, postgres or sqlserver); GATEWAY_HOST replaces the image repository prefix.
GATEWAY_VERSION ?= 1.2.0
IT_BLOCK = gateway-controller-policies$(if $(DB),/$(DB))

it: ## Run the policy integration tests against a released gateway
	cd tests/it && go test . -count=1 -timeout=40m -v -run TestIntegrationSuite -args \
		-blocks=$(IT_BLOCK) \
		$(if $(POLICIES),-policies=$(POLICIES)) \
		-gateway-version=$(GATEWAY_VERSION) \
		$(if $(GATEWAY_HOST),-host=$(GATEWAY_HOST))

it-unit: ## Run the unit tests of the integration suite's step definitions
	cd tests/it && go test ./steps/... -count=1

it-standards: ## Check the step definitions and every policy feature against the framework standards
	cd tests/it && go tool standards -root . -unit-root . -steps steps \
		-features $$(ls -d ../../policies/*/it/features | paste -sd, -) \
		-scripts "" -suites it-suite.yaml -docs "" \
		-approved-imports github.com/wso2/gateway-controllers/tests/it/

it-scaffold: ## Create the integration test skeleton for POLICY (it/it.yaml and a feature)
	@test -n "$(POLICY)" || { echo "usage: make it-scaffold POLICY=<policy name>"; exit 2; }
	python3 scripts/it/scaffold.py $(POLICY)
