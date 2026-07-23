---
title: "Overview"
---
# MCP Authorization

## Overview

The MCP Authorization policy provides fine-grained access control for Model Context Protocol (MCP) server resources. It enables API administrators to define authorization rules based on user claims and scopes extracted from validated JWT tokens, controlling access to specific MCP tools, resources, prompts, and JSON-RPC methods.

> **Prerequisite**: The MCP Authorization policy requires the [MCP Authentication policy](./mcp-authentication.md) to be applied first. The MCP Authentication policy validates and extracts JWT claims that are used by the authorization policy for access control decisions.

## Features

- **Tool-Level Access Control**: Restrict access to specific MCP tools based on user claims and scopes
- **Resource-Level Access Control**: Control access to specific MCP resources based on authorization rules
- **Prompt-Level Access Control**: Manage access to specific MCP prompts
- **JSON-RPC Method-Level Access Control**: Apply authorization rules at the JSON-RPC method level (e.g., `tools/call`, `resources/read`, `prompts/get`) for fine-grained control
- **Flexible Rule-Based Authorization**: Define multiple authorization rules with name matching (exact or wildcard `*`)
- **Claim-Based Validation**: Validate custom claims (e.g., department, role, team) in user tokens
- **Scope-Based Validation**: Require specific OAuth scopes for accessing protected resources
- **Boolean Composition (`allOf` / `anyOf`)**: Express scope and claim requirements with `allOf` (all) and/or `anyOf` (at least one) via the `scopes` and `claims` fields
- **Wildcard Matching**: Use wildcard patterns (`*`) to create default rules for all resources of a type
- **Rule Specificity**: Exact-name rules are evaluated before wildcards, and all matching rules must pass

## Configuration

The MCP Authorization policy uses a single-level configuration model where all parameters are configured per-MCP-API/route in the API definition YAML.

### User Parameters (API Definition)

These parameters are configured per MCP Proxy by the API developer:

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `tools` | `AuthzRule` array | No | `[]` | Authorization rules for MCP tools. |
| `resources` | `AuthzRule` array | No | `[]` | Authorization rules for MCP resources. |
| `prompts` | `AuthzRule` array | No | `[]` | Authorization rules for MCP prompts. |
| `methods` | `AuthzRule` array | No | `[]` | Authorization rules for MCP (JSON-RPC) methods. |

### AuthzRule Configuration

Each authorization rule object supports the following fields:

> Each rule must specify at least one authorization condition: `scopes`, `claims`, or the deprecated `requiredScopes` / `requiredClaims`. When several are present, all of them must pass.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | Yes | `"*"` | Name of the resource to authorize, or `*` to match all resources of this type (1–256 characters). |
| `scopes` | object | Conditional | `{}` | Scope requirement as `allOf` (every listed scope must be present) and/or `anyOf` (at least one must be present); AND-ed when both are given. Takes precedence over `requiredScopes`. See [Example 6](#example-6-scopes-and-claims-each-with-allof--anyof). |
| `claims` | object | Conditional | `{}` | Claim requirement as `allOf` and/or `anyOf` lists of matchers, each `{ claim, values }`. A matcher is satisfied when the claim's value is one of `values`; `allOf` requires every matcher, `anyOf` at least one, AND-ed when both are given. Takes precedence over `requiredClaims`. See [Example 6](#example-6-scopes-and-claims-each-with-allof--anyof). |
| `requiredScopes` | string array | Conditional | `[]` | **Deprecated — use `scopes` (`anyOf`) instead.** At least one of the listed scopes must be present. Ignored when `scopes` is set. |
| `requiredClaims` | object | Conditional | `{}` | **Deprecated — use `claims` instead.** Map of claim names to expected values; all must be present and match exactly. Ignored when `claims` is set. |

**Note:**

Inside the `gateway/build.yaml`, ensure the policy module is added under `policies:`:

```yaml
- name: mcp-authz
  gomodule: github.com/wso2/gateway-controllers/policies/mcp-authz@v1
```

## Reference Scenarios

### Example 1: Basic Tool Access Control

Restrict access to specific tools based on scopes:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: Mcp
metadata:
  name: mcp-server-api-v1.0
spec:
  displayName: mcp-server-api
  version: v1.0
  context: /mcpserver
  vhost: mcp1.gw.example.com
  upstream:
    url: https://mcp-backend:8080
  policies:
    - name: mcp-auth
      version: v1
      params:
        issuers:
          - PrimaryIDP
    - name: mcp-authz
      version: v1
      params:
        tools:
          - name: list_files
            requiredScopes:
              - mcp:tool:read
          - name: create_file
            requiredScopes:
              - mcp:tool:write
          - name: "*"
            requiredScopes:
              - mcp:tool:execute
  tools:
    ...
```

**Authorization decision examples:**

**Scenario 1**: User with scope `mcp:tool:read` attempts to call `list_files` tool
- Rule: `name="list_files", requiredScopes=["mcp:tool:read"]`
- Result: ✅ Access Granted

**Scenario 2**: User with scope `mcp:tool:execute` (no write scope) attempts to call `create_file` tool
- Rule: `name="create_file", requiredScopes=["mcp:tool:write"]`
- Result: ❌ Access Denied (insufficient scopes)

**Scenario 3**: User with scopes `mcp:tool:write` and `mcp:tool:execute` attempts to call `create_file` tool
- Matching rules:
  - `name="create_file", requiredScopes=["mcp:tool:write"]`
  - `name="*", requiredScopes=["mcp:tool:execute"]`
- Result: ✅ Access Granted (both matching rules pass)

### Example 2: Claim-Based Resource Access

Control resource access based on user claims:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: Mcp
metadata:
  name: mcp-server-api-v1.0
spec:
  displayName: mcp-server-api
  version: v1.0
  context: /mcpserver
  vhost: mcp1.gw.example.com
  upstream:
    url: https://mcp-backend:8080
  policies:
    - name: mcp-auth
      version: v1
      params:
        issuers:
          - PrimaryIDP
    - name: mcp-authz
      version: v1
      params:
        resources:
          - name: "file:///private/main"
            requiredClaims:
              department: "engineering"
            requiredScopes:
              - mcp:resource:read
          - name: "file:///public/main"
            requiredScopes:
              - mcp:resource:read
  tools:
    ...
```

**Authorization decision examples:**

**Scenario 4**: User with claim `department="engineering"` and scope `mcp:resource:read` attempts to read resource `file:///private/main`
- Rule: `name="file:///private/main", requiredClaims={department="engineering"}, requiredScopes=["mcp:resource:read"]`
- Result: ✅ Access Granted

**Scenario 5**: User with claim `department="finance"` (no engineering) and scope `mcp:resource:read` attempts to read resource `file:///private/main`
- Rule: `name="file:///private/main", requiredClaims={department="engineering"}, requiredScopes=["mcp:resource:read"]`
- Result: ❌ Access Denied (claim mismatch)

**Scenario 6**: User with claim `department="engineering"` but no `mcp:resource:read` scope attempts to read resource `file:///private/main`
- Rule: `name="file:///private/main", requiredClaims={department="engineering"}, requiredScopes=["mcp:resource:read"]`
- Result: ❌ Access Denied (scope mismatch)

### Example 3: Role-Based Prompt Access

Restrict prompt access based on user roles:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: Mcp
metadata:
  name: mcp-server-api-v1.0
spec:
  displayName: mcp-server-api
  version: v1.0
  context: /mcpserver
  vhost: mcp1.gw.example.com
  upstream:
    url: https://mcp-backend:8080
  policies:
    - name: mcp-auth
      version: v1
      params:
        issuers:
          - PrimaryIDP
    - name: mcp-authz
      version: v1
      params:
        prompts:
          - name: "admin_summary"
            requiredClaims:
              role: "admin"
            requiredScopes:
              - mcp:prompt:admin
          - name: "*"
            requiredScopes:
              - mcp:prompt:read
  tools:
    ...
```

### Example 4: Method-Level Authorization

Apply authorization at the JSON-RPC method level:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: Mcp
metadata:
  name: mcp-server-api-v1.0
spec:
  displayName: mcp-server-api
  version: v1.0
  context: /mcpserver
  vhost: mcp1.gw.example.com
  upstream:
    url: https://mcp-backend:8080
  policies:
    - name: mcp-auth
      version: v1
      params:
        issuers:
          - PrimaryIDP
    - name: mcp-authz
      version: v1
      params:
        methods:
          - name: "tools/call"
            requiredScopes:
              - mcp:method:tools:call
          - name: "resources/write"
            requiredClaims:
              role: "admin"
            requiredScopes:
              - mcp:method:resources:write
          - name: "*"
            requiredScopes:
              - mcp:method:general
  tools:
    ...
```

### Example 5: Multi-Level Authorization

Combine different resource types with varying access requirements:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: Mcp
metadata:
  name: mcp-server-api-v1.0
spec:
  displayName: mcp-server-api
  version: v1.0
  context: /mcpserver
  vhost: mcp1.gw.example.com
  upstream:
    url: https://mcp-backend:8080
  policies:
    - name: mcp-auth
      version: v1
      params:
        issuers:
          - PrimaryIDP
        requiredScopes:
          - mcp:access
    - name: mcp-authz
      version: v1
      params:
        # Restrictive tool access
        tools:
          - name: "execute_command"
            requiredClaims:
              department: "platform"
              role: "admin"
            requiredScopes:
              - mcp:tool:execute:admin
          - name: "*"
            requiredScopes:
              - mcp:tool:execute
        # Resource access for a finance document (rule names are matched exactly, or a standalone "*")
        resources:
          - name: "file:///finance/report.pdf"
            requiredClaims:
              department: "finance"
            requiredScopes:
              - mcp:resource:read:finance
          - name: "*"
            requiredScopes:
              - mcp:resource:read
        # Prompt access
        prompts:
          - name: "admin_dashboard"
            requiredClaims:
              role: "admin"
            requiredScopes:
              - mcp:prompt:admin
  tools:
    ...
```

### Example 6: Scopes and Claims, each with `allOf` + `anyOf`


```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: Mcp
metadata:
  name: mcp-authz-scopes-claims
spec:
  displayName: MCP AuthZ Scopes And Claims
  version: v1.0
  context: /mcpauthzscopesclaims
  specVersion: "2025-06-18"
  upstream:
    url: http://mcp-server-backend:3001/mcp
  policies:
    - name: mcp-auth
      version: v1
      params:
        issuers:
          - PrimaryIDP
    - name: mcp-authz
      version: v1
      params:
        tools:
          - name: deploy
            scopes:
              allOf:
                - "mcp:tool:read"
                - "mcp:tool:list"
              anyOf:
                - "mcp:tool:write"
                - "mcp:tool:admin"
            claims:
              allOf:
                - claim: department
                  values: ["engineering"]
              anyOf:
                - claim: role
                  values: ["lead", "staff"]
```

A `tools/call` for the `deploy` tool is authorized only when **all** of the following hold:

```text
SCOPES = (mcp:tool:read AND mcp:tool:list)  AND  (mcp:tool:write OR mcp:tool:admin)
CLAIMS = (department = engineering)         AND  (role ∈ {lead, staff})
```

| Token scopes | `department` | `role` | Result | Reason |
| --- | --- | --- | --- | --- |
| `mcp:tool:read mcp:tool:list mcp:tool:write` | `engineering` | `lead` | Authorized | scope `allOf` + `anyOf` and claim `allOf` + `anyOf` all satisfied |
| `mcp:tool:read mcp:tool:list` | `engineering` | `lead` | 403 | scope `anyOf` unmet — neither `mcp:tool:write` nor `mcp:tool:admin` present |
| `mcp:tool:read mcp:tool:write` | `engineering` | `lead` | 403 | scope `allOf` incomplete — `mcp:tool:list` missing |
| `mcp:tool:read mcp:tool:list mcp:tool:admin` | `sales` | `lead` | 403 | claim `allOf` unmet — `department` is not `engineering` |
| `mcp:tool:read mcp:tool:list mcp:tool:admin` | `engineering` | `intern` | 403 | claim `anyOf` unmet — `role` not in `{lead, staff}` |

**Precedence and deprecation:** New `scopes`/`claims` fields take precedence over their deprecated counterparts (`requiredScopes`/`requiredClaims`) per dimension; legacy fields remain supported with deprecation warnings at policy load. Any malformed `scopes` or `claims` value fails the policy at load.

## Authorization Logic

The MCP Authorization policy evaluates rules using the following logic:

1. **Specificity Ordering**: Exact-name rules are evaluated before wildcard (`*`) rules
2. **Match All**: All rules that match the resource name are evaluated
3. **All Must Pass**: For access to be granted, all matching rules must pass (both claims and scopes)
4. **Default Allow**: If no rules match the resource, access is allowed (default permit)

### Examples

| Resource | Rules Defined | User Token | Result |
|----------|--------------|------------|--------|
| `list_files` | `[{name:"list_files", scopes:["read"]}, {name:"*", scopes:["execute"]}]` | Scopes: `["read", "execute"]` | ✅ Allowed |
| `list_files` | `[{name:"list_files", scopes:["read"]}, {name:"*", scopes:["execute"]}]` | Scopes: `["execute"]` | ❌ Denied (exact rule fails) |
| `delete_file` | `[{name:"list_files", scopes:["read"]}, {name:"*", scopes:["execute"]}]` | Scopes: `["execute"]` | ✅ Allowed (only wildcard matches) |
| `admin_tool` | `[{name:"admin_tool", claims:{role:"admin"}, scopes:["write"]}]` | Claims: `{role:"admin"}`, Scopes: `["write"]` | ✅ Allowed |
| `admin_tool` | `[{name:"admin_tool", claims:{role:"admin"}, scopes:["write"]}]` | Claims: `{role:"user"}`, Scopes: `["write"]` | ❌ Denied (claim mismatch) |

## Notes

When authorization fails, the policy returns:
- **HTTP Status**: `403 Forbidden`
- **Response Body**: JSON error response with a reason message
- **WWW-Authenticate Header**: Contains information about required scopes for the denied resource

## Related Policies

- [MCP Authentication Policy](./mcp-authentication.md) - Validates JWT tokens and is a prerequisite for MCP Authorization
- [JWT Authentication Policy](../../../gateway/policies/jwt-authentication.md) - Base JWT token validation mechanism
