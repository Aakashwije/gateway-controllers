---
title: "Overview"
---
# Redirect

## Overview

The Redirect policy issues an HTTP redirect to the client without forwarding the request to the upstream backend. It runs in the request phase, builds a `Location` header, and short-circuits the request with the configured redirect status code. This implements the Gateway-API `RequestRedirect` filter semantics.

Only the components you explicitly configure are overridden. Every component left unset — scheme, host, path, and query string — is preserved from the incoming request. The port is preserved too, except that overriding the scheme without also setting a port switches to the target scheme's default port. Because the redirect is produced at the gateway, no upstream call is made and response-phase policies are not involved for that request path.

## Features

- Returns HTTP redirects from the gateway request phase (no upstream invocation)
- Configurable redirect status code (`301`, `302`, `303`, `307`, `308`; default `302`)
- Optional scheme override (`http` or `https`); unset preserves the request scheme
- Optional hostname override; unset preserves the request `Host`
- Optional port override; unset preserves the request port or the scheme's well-known port
- Optional path rewrite with full-replacement or prefix-replacement modes
- Query string is always preserved from the original request
- Component-preserving `Location` construction with scheme-aware default-port omission

## Configuration

The Redirect policy uses single-level API definition parameters and does not require any system-level configuration.

### User Parameters (API Definition)

These parameters are configured per-API/route by the API developer:

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `statusCode` | integer | No | `302` | HTTP redirect status code. Allowed values: `301`, `302`, `303`, `307`, `308`. |
| `scheme` | string | No | *(request scheme)* | Redirect scheme. Allowed values: `http`, `https`. Unset preserves the request scheme. |
| `hostname` | string | No | *(request Host)* | Redirect host. Length: 1-253 characters; must match pattern `^[a-zA-Z0-9.-]+$`. Unset preserves the request `Host`. |
| `port` | integer | No | *(request port)* | Redirect port. Range: 1-65535. Unset preserves the request port, or uses the well-known port of the redirect scheme (`80` for `http`, `443` for `https`) when the scheme is changed. |
| `path` | object | No | *(request path)* | Path rewrite for the redirect. Unset preserves the request path. |

### Path Object

When `path` is set, both fields are required:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `mode` | string | Yes | How `value` rewrites the path. `full` replaces the entire request path with `value`. `prefix` replaces only the portion of the path that matched the operation's path prefix with `value`, preserving the remaining suffix. |
| `value` | string | Yes | Replacement path. Must start with `/`. Maximum length: 8192 characters. |

**Note:**

Inside the `gateway/build.yaml`, ensure the policy module is added under `policies:`:

```yaml
- name: redirect
  gomodule: github.com/wso2/gateway-controllers/policies/redirect@v0
```

## Reference Scenarios:

### Example 1: Redirect to a Different Host (Default 302)

Redirect all traffic for an API to a different host, preserving scheme, port, path, and query:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: store-api-v1.0
spec:
  displayName: Store-API
  version: v1.0
  context: /store/$version
  upstream:
    main:
      url: http://sample-backend:5000
  policies:
    - name: redirect
      version: v0
      params:
        hostname: example.org
  operations:
    - method: GET
      path: /products
```

**Response behavior:**

Incoming client request
```http
GET /store/v1.0/products?page=2 HTTP/1.1
Host: api.company.com
```

Immediate gateway response
```http
HTTP/1.1 302 Found
location: http://example.org/store/v1.0/products?page=2
```

### Example 2: Permanent Redirect with Host and Status

Return a permanent `301` redirect to a new host:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: legacy-api-v1.0
spec:
  displayName: Legacy-API
  version: v1.0
  context: /legacy/$version
  upstream:
    main:
      url: http://sample-backend:5000
  policies:
    - name: redirect
      version: v0
      params:
        statusCode: 301
        hostname: example.org
  operations:
    - method: GET
      path: /catalog
```

**Response behavior:**

Incoming client request
```http
GET /legacy/v1.0/catalog HTTP/1.1
Host: api.company.com
```

Immediate gateway response
```http
HTTP/1.1 301 Moved Permanently
location: http://example.org/legacy/v1.0/catalog
```

### Example 3: HTTP to HTTPS Upgrade

Redirect insecure requests to `https`. Because the scheme changes and no explicit port is set, the scheme's default port (`443`) is used and omitted from the `Location`:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: secure-api-v1.0
spec:
  displayName: Secure-API
  version: v1.0
  context: /secure/$version
  upstream:
    main:
      url: http://sample-backend:5000
  policies:
    - name: redirect
      version: v0
      params:
        scheme: https
  operations:
    - method: GET
      path: /data
```

**Response behavior:**

Incoming client request
```http
GET /secure/v1.0/data HTTP/1.1
Host: api.company.com
```

Immediate gateway response
```http
HTTP/1.1 302 Found
location: https://api.company.com/secure/v1.0/data
```

### Example 4: Full Path Replacement

Replace the entire request path while redirecting to a new host:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: moved-api-v1.0
spec:
  displayName: Moved-API
  version: v1.0
  context: /store/$version
  upstream:
    main:
      url: http://sample-backend:5000
  policies:
    - name: redirect
      version: v0
      params:
        statusCode: 308
        hostname: example.org
        path:
          mode: full
          value: /v2/catalog
  operations:
    - method: GET
      path: /products
```

**Response behavior:**

Incoming client request
```http
GET /store/v1.0/products HTTP/1.1
Host: api.company.com
```

Immediate gateway response
```http
HTTP/1.1 308 Permanent Redirect
location: http://example.org/v2/catalog
```

### Example 5: Prefix Path Replacement

Replace only the matched path prefix, preserving the remaining suffix (canonical URL rewrite on the same host). The operation matches a prefix, and `prefix` mode swaps `/shoes` for `/footwear`:

```yaml
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: catalog-api-v1.0
spec:
  displayName: Catalog-API
  version: v1.0
  context: /
  upstream:
    main:
      url: http://sample-backend:5000
  operations:
    - method: GET
      path: /shoes/*
      policies:
        - name: redirect
          version: v0
          params:
            path:
              mode: prefix
              value: /footwear
```

**Response behavior:**

Incoming client request
```http
GET /shoes/nike-air HTTP/1.1
Host: api.company.com
```

Immediate gateway response
```http
HTTP/1.1 302 Found
location: http://api.company.com/footwear/nike-air
```

## How it Works

* The policy executes in the request phase and returns a `policy.ImmediateResponse` carrying the redirect status code and a `Location` header; the upstream backend is never called.
* The `Location` is assembled from the incoming request, overriding only the components set in configuration:
  * **Scheme** — `scheme` if set, otherwise the request scheme (defaults to `http` when absent).
  * **Host** — `hostname` if set, otherwise the request `Host` (falling back to the route's virtual host).
  * **Port** — `port` if set (omitted when it equals the scheme's default). If only the scheme changed, the new scheme's default port is used and omitted. Otherwise the request port is preserved (and omitted when it equals the scheme default).
  * **Path** — rewritten per the `path` block (`full` or `prefix`), otherwise the request path is preserved unchanged.
  * **Query** — always carried over from the original request.
* If `statusCode` is omitted, the redirect status defaults to `302`.
* Configuration is validated when the policy chain is built, so invalid parameters (bad status code, empty hostname, out-of-range port, malformed `path`) surface at deployment time rather than per request. A defensive runtime fallback returns a `500 Configuration Error` response.

## Limitations

1. **Short-Circuit Behavior**: The upstream backend is never invoked when this policy executes.
2. **Request-Phase Only**: Response-phase policy logic is not applicable for requests terminated by this policy.
3. **Client Follow-Through**: The redirect only takes effect if the client honors `3xx` responses and the `Location` header.
4. **Prefix Mode Scope**: `prefix` path mode replaces the prefix matched by the operation's path; it is intended for operations that match on a path prefix.
5. **Hostname Format**: `hostname` must match `^[a-zA-Z0-9.-]+$`; it does not accept a scheme, port, or path — configure those via the dedicated parameters.

## Notes

**Operational Usage**

Use this policy to move traffic to a new host, upgrade clients to `https`, or rewrite paths to a canonical location without changing backend deployments. Choose `301`/`308` for permanent moves (cacheable by clients) and `302`/`303`/`307` for temporary redirects. Apply route-level attachment when only specific operations should be redirected.

**Security and Governance**

Configure redirect targets with trusted host values, and restrict who can modify this policy's parameters through your standard configuration-governance process.

**Performance and Reliability**

Because the redirect is produced at the gateway, it adds negligible latency and does not depend on upstream availability, making it safe to use during migrations or incidents.
