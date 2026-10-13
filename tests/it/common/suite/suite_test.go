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

package suite

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestStagePolicyCopiesTheWholePolicyExceptItsIntegrationTests(t *testing.T) {
	repo := t.TempDir()
	policy := filepath.Join(repo, "policies", "sample-policy")
	writeFile(t, filepath.Join(policy, "policy-definition.yaml"), "name: sample-policy\n")
	writeFile(t, filepath.Join(policy, "go.mod"), "module sample\n")
	writeFile(t, filepath.Join(policy, "internal", "logic.go"), "package internal\n")
	writeFile(t, filepath.Join(policy, "it", "it-suite.yaml"), "suite: sample\n")
	writeFile(t, filepath.Join(policy, "it", "go.mod"), "module sample/it\n")
	writeFile(t, filepath.Join(repo, "policies", "other-policy", "policy-definition.yaml"), "name: other-policy\n")
	workspace := t.TempDir()

	require.NoError(t, StagePolicy(filepath.Join(policy, "it"), workspace))

	staged := filepath.Join(workspace, "policies", "sample-policy")
	require.FileExists(t, filepath.Join(staged, "policy-definition.yaml"))
	require.FileExists(t, filepath.Join(staged, "go.mod"))
	require.FileExists(t, filepath.Join(staged, "internal", "logic.go"))
	require.NoDirExists(t, filepath.Join(staged, "it"))

	entries, err := os.ReadDir(filepath.Join(workspace, "policies"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the staged tree holds only this policy")
}

func TestStagePolicyAddsTheCompanionPolicies(t *testing.T) {
	repo := t.TempDir()
	for _, name := range []string{"sample-policy", "set-headers", "respond"} {
		writeFile(t, filepath.Join(repo, "policies", name, "policy-definition.yaml"), "name: "+name+"\n")
		writeFile(t, filepath.Join(repo, "policies", name, "it", "it-suite.yaml"), "suite: "+name+"\n")
	}
	writeFile(t, filepath.Join(repo, "policies", "unused", "policy-definition.yaml"), "name: unused\n")
	workspace := t.TempDir()

	// A companion named twice, or the policy itself, is staged once.
	require.NoError(t, StagePolicy(filepath.Join(repo, "policies", "sample-policy", "it"), workspace,
		"set-headers", "respond", "set-headers", "sample-policy"))

	entries, err := os.ReadDir(filepath.Join(workspace, "policies"))
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Equal(t, []string{"respond", "sample-policy", "set-headers"}, names)
	require.NoDirExists(t, filepath.Join(workspace, "policies", "respond", "it"))

	err = StagePolicy(filepath.Join(repo, "policies", "sample-policy", "it"), t.TempDir(), "missing")
	require.ErrorContains(t, err, "not a policy folder")
}

func TestControllerPoliciesAreTheOnesTheControllerRequires(t *testing.T) {
	require.ElementsMatch(t,
		[]string{"api-key-auth", "host-rewrite", "oauth2-generator", "respond", "set-headers"}, ControllerPolicies)
}

func TestStagePolicyRejectsWhatIsNotAPolicyIntegrationFolder(t *testing.T) {
	repo := t.TempDir()
	policy := filepath.Join(repo, "policies", "sample-policy")
	writeFile(t, filepath.Join(policy, "it", "it-suite.yaml"), "suite: sample\n")

	require.ErrorContains(t, StagePolicy(filepath.Join(policy, "it"), t.TempDir()), "not a policy folder")
	require.ErrorContains(t, StagePolicy(policy, t.TempDir()), "not a policy's it folder")
}

func TestStagePolicyRefusesSymlinks(t *testing.T) {
	repo := t.TempDir()
	policy := filepath.Join(repo, "policies", "sample-policy")
	writeFile(t, filepath.Join(policy, "policy-definition.yaml"), "name: sample-policy\n")
	writeFile(t, filepath.Join(policy, "it", "it-suite.yaml"), "suite: sample\n")
	require.NoError(t, os.Symlink(filepath.Join(policy, "policy-definition.yaml"), filepath.Join(policy, "link.yaml")))

	require.ErrorContains(t, StagePolicy(filepath.Join(policy, "it"), t.TempDir()), "symlink")
}
