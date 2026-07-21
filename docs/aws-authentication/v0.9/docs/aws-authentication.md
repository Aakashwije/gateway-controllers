---
title: "Overview"
---
# AWS Authentication

## Overview

The **AWS Authentication** policy signs outbound requests to AWS-hosted backends
using [AWS Signature Version 4 (SigV4)](https://docs.aws.amazon.com/general/latest/gr/signature-version-4.html)
before they are forwarded upstream. Many AWS services — API Gateway endpoints
protected with IAM authorization, Lambda Function URLs, OpenSearch domains, S3,
and others — reject requests that are not correctly SigV4-signed. This policy
lets the gateway act as a trusted caller to such backends by signing every
proxied request with credentials configured directly on the API.

Two credential-acquisition modes are supported:

- **`sts-assume-role`** — the gateway calls AWS STS `AssumeRole` to obtain
  short-lived temporary credentials, then signs with those. Temporary
  credentials are cached and automatically refreshed before they expire.
- **`iam-user-access-key`** — the gateway signs directly with a static,
  long-lived IAM user access key/secret pair (optionally paired with a
  session token if the configured key is already temporary).

## Features

- AWS SigV4 request signing for any AWS service (`execute-api`, `lambda`, `s3`, `es`, ...)
- Two credential modes: STS AssumeRole and static IAM user access keys
- Automatic caching and refresh of temporary credentials obtained via AssumeRole
- Cross-account role assumption support via an optional External ID
- Preserves any existing authentication context set by an earlier (inbound) auth policy

## Configuration

### User Parameters (API Definition)

All configuration for this policy — including credential material — is set
per-API in the API definition. There are no system/config.toml-level
parameters.

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `service` | string | Yes | | AWS SigV4 signing name of the target backend service (e.g. `execute-api`, `lambda`, `es`, `s3`). |
| `region` | string | Yes | | AWS region of the target backend (e.g. `us-east-1`). |
| `authenticationType` | string | Yes | | Selects the credential-acquisition mode: `sts-assume-role` or `iam-user-access-key`. |
| `awsAccessKeyID` | string | Conditional | | Required when `authenticationType` is `iam-user-access-key`. Optional base credential for `sts-assume-role` (if omitted, the default AWS SDK credential chain is used to call `sts:AssumeRole`). |
| `awsSecretAccessKey` | string | Conditional | | Required when `authenticationType` is `iam-user-access-key`. Must be set together with `awsAccessKeyID`. |
| `awsSessionToken` | string | No | | Optional session token, only meaningful when `awsAccessKeyID`/`awsSecretAccessKey` already represent temporary credentials. |
| `awsRoleARN` | string | Conditional | | Required when `authenticationType` is `sts-assume-role`. ARN of the IAM role to assume. |
| `awsRoleExternalID` | string | No | | Optional External ID passed to `sts:AssumeRole`, for cross-account role assumption hardening. Only applicable for `sts-assume-role`. |
| `awsRoleSessionName` | string | No | `aws-authentication-session` | Session name used when assuming the role. Only applicable for `sts-assume-role`. |

> **Security note:** Because credential material is set directly on the API's
> own policy configuration (rather than resolved from operator-controlled
> gateway configuration), access to API definitions containing this policy
> should be restricted accordingly. Scope the IAM user or role used to the
> minimum permissions required by the target backend (least privilege), and
> prefer `sts-assume-role` with short-lived credentials over long-lived IAM
> user access keys wherever possible.

**Note:**

Inside the `gateway/build.yaml`, ensure the policy module is added under `policies:`:

```yaml
- name: aws-authentication
  gomodule: github.com/wso2/gateway-controllers/policies/aws-authentication@v0
```

## How It Works

1. **Body phase** – Because SigV4 requires a hash of the full request body,
   this policy buffers the complete request before processing.
2. The policy retrieves current AWS credentials from the provider configured
   at policy-instance creation time:
   - `iam-user-access-key` mode uses the static access key/secret directly.
   - `sts-assume-role` mode calls AWS STS `AssumeRole` (caching and
     automatically refreshing the resulting temporary credentials across
     requests until they are close to expiry).
3. A synthetic HTTP request mirroring the proxied request (method, path,
   query string, host, and body) is built and signed with SigV4 using the
   configured `service` and `region`.
4. The resulting `Authorization`, `X-Amz-Date`, `X-Amz-Content-Sha256`, and
   (when using temporary credentials) `X-Amz-Security-Token` headers are set
   on the request before it is forwarded upstream. The request body is never
   modified.
5. If credentials cannot be retrieved or signing fails, the request is
   short-circuited with `502 Bad Gateway` — this reflects a gateway-to-backend
   authentication failure, not an inbound-client authentication rejection.

## Reference Scenarios

### Example 1: IAM User Access Keys Against an API Gateway (IAM Auth) Backend

```yaml
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: inventory-api-v1.0
spec:
  displayName: Inventory-API
  version: v1.0
  context: /inventory/$version
  upstream:
    main:
      url: https://abc123xyz.execute-api.us-east-1.amazonaws.com/prod
  policies:
    - name: aws-authentication
      version: v0
      params:
        service: execute-api
        region: us-east-1
        authenticationType: iam-user-access-key
        awsAccessKeyID: AKIAEXAMPLE1234567
        awsSecretAccessKey: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
  operations:
    - method: GET
      path: /items
    - method: POST
      path: /items
```

### Example 2: STS AssumeRole Against a Lambda Function URL

```yaml
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: orders-api-v1.0
spec:
  displayName: Orders-API
  version: v1.0
  context: /orders/$version
  upstream:
    main:
      url: https://abcdefghij.lambda-url.us-east-1.on.aws
  policies:
    - name: aws-authentication
      version: v0
      params:
        service: lambda
        region: us-east-1
        authenticationType: sts-assume-role
        awsRoleARN: arn:aws:iam::123456789012:role/gateway-orders-invoker
        awsRoleExternalID: gateway-prod-01
  operations:
    - method: GET
      path: /{orderId}
    - method: POST
      path: /
```

### Example 3: Cross-Account Role Assumption with Base Credentials

```yaml
  policies:
    - name: aws-authentication
      version: v0
      params:
        service: es
        region: eu-west-1
        authenticationType: sts-assume-role
        awsAccessKeyID: AKIAEXAMPLE1234567
        awsSecretAccessKey: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
        awsRoleARN: arn:aws:iam::987654321098:role/cross-account-opensearch-access
        awsRoleExternalID: partner-gateway-search
        awsRoleSessionName: gateway-search-session
```

## Error Responses

All error responses are returned as JSON with `Content-Type: application/json`.

| Scenario | Status | Message |
|----------|--------|---------|
| AWS credentials could not be retrieved (e.g. AssumeRole call failed) | 502 | `failed to authenticate request to upstream AWS service` |
| SigV4 signing failed | 502 | `failed to authenticate request to upstream AWS service` |

**Example error body:**
```json
{
  "error": "Bad Gateway",
  "message": "failed to authenticate request to upstream AWS service"
}
```

## Security Considerations

- **Least privilege** – Scope the IAM user or role used by this policy to only
  the permissions required by the target AWS backend.
- **Prefer temporary credentials** – Where possible, use `sts-assume-role`
  instead of long-lived `iam-user-access-key` credentials, since assumed-role
  credentials are automatically short-lived and rotated.
- **Cross-account hardening** – When assuming a role in another AWS account,
  always set `awsRoleExternalID` to guard against the confused deputy problem.
- **Credential storage** – Credential material configured on this policy is
  stored as part of the API's own configuration. Restrict access to API
  definitions and control-plane storage accordingly.
- **HTTPS only** – Ensure the upstream AWS backend URL uses HTTPS so that
  signed requests (and any response data) are not exposed to network
  eavesdroppers.

## Gateway Module Reference

```yaml
- name: aws-authentication
  gomodule: github.com/wso2/gateway-controllers/policies/aws-authentication@v0
```
