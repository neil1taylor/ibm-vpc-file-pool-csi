# Spec for #46 item 4: receiver mode setup in the console

Date: 2026-10-21. Status: draft for review.

## Goal

An operator can set up replication to a receiver (driver-to-driver mode) from the console, with no YAML and no `kubectl`. "Done" means: the form makes the token Secret and the CA Secret, tests that the receiver answers and accepts the token, and only then creates the ReplicationPolicy. A wrong address, a wrong token or a wrong certificate shows a plain message before the first copy is due, not as a failed cycle an hour later.

## What exists today

- The create form has the receiver address, the name of the token Secret and the name of the CA Secret as plain text fields. The operator must make both Secrets first, in the `kube-system` namespace of the source cluster, with the keys `token` and `ca.crt`. Nothing checks them.
- The receiver has one open health call (`GET /api/v1/health`). It checks that the receiver's NFS path is reachable. It does not check the token or the certificate, because it needs neither.
- The destination base path is a field with the default `/pvcs`.
- A mistake shows up late: the first sync-client Job fails, the policy shows an error, and the operator reads Job logs.

## Design

### 1. The check cannot run in the browser

The receiver is on another cluster and often has a self-signed certificate. A browser call to it fails on certificate trust and on cross-origin rules, and it would send the token from the operator's laptop. So the check runs **inside the source cluster**, the same place the real copies run.

### 2. One new receiver call and one new check command (driver change)

- The receiver gets `GET /api/v1/auth-check`. It needs the bearer token and returns `200` with the base path and the receiver's version. It changes nothing. A wrong token gets `401`.
- The sync-client binary (already in the driver image) gets a `check` command with the same flags as a copy (`--endpoint`, `--token-file`, `--ca-cert-file`). It calls the health call, then the auth-check call, and prints one result line: `ok`, or a short reason (`cannot connect`, `certificate not trusted`, `token refused`, `receiver storage not reachable`). The exit code is 0 only for `ok`.

### 3. The console flow

The "Receiver" part of the form becomes a guided step:

1. **Address.** HTTPS URL. The form refuses `http://` with a plain reason.
2. **Token.** A password field. The operator pastes the token (it is in the receiver's Secret on the destination cluster; the form says where to find it and gives the one command).
3. **Certificate.** A text box for the CA certificate (PEM), with the note "leave empty when the receiver's certificate comes from a public authority". The form checks that the text starts with `-----BEGIN CERTIFICATE-----`.
4. **Base path.** Default `/pvcs`. Unchanged.
5. **Check connection.** The console creates the two Secrets (named `<policy name>-receiver-token` and `<policy name>-receiver-ca`, labelled as made by the console) and one Job in `kube-system` that runs `sync-client check` from the driver image. The form shows the result line. On success the **Create** button turns on. On failure the form shows the reason, and the operator fixes the field and checks again.
6. **Create.** Makes the ReplicationPolicy with the two Secret names filled in. The Job is deleted when the check ends (a short time-to-live is set on it, in case the page is closed).

If the operator already has Secrets, the form still accepts their names (the form today). Then the check runs against those Secrets, and the console makes no new ones.

### 4. What the console never does

- It never reads a Secret back. It can only create one. The token is sent once, from the browser to the source cluster's API server, and is not kept anywhere else (not in browser storage, not in a log).
- It never changes the destination cluster. Setting up the receiver itself (Helm values, Route, certificate) stays a documented task. A later spec can add a "destination checklist" page.
- It never deletes a Secret it did not make. A Secret it made is deleted with the policy only if the operator ticks a box on the delete dialog.

### 5. Access

Making a Secret and a Job in `kube-system` needs rights the operator may not have. The form asks the cluster if the operator may (an access review). If not, it says which objects are needed and shows the YAML to give to an administrator. It does not fail half way.

## Plan

1. Driver: the receiver's `auth-check` call and the sync-client `check` command, with tests (Go, table tests, a TLS test with a self-signed certificate, as the receiver tests have today). One PR.
2. Console: the guided step, the check Job and the access review, with tests. One PR.
3. Release test: a check against a real receiver. This needs two clusters, so it belongs to the replication stage (10).

About 2 PRs plus the release stage. Docs: `CROSS-REGION-DR.md` (the setup steps get a console route), `CRD-SPEC.md` (no change to the CRD), `FEATURES.md`, `CHANGELOG.md`.

## Cost and risk

- Low risk to data: nothing here copies data. The new receiver call changes nothing and needs the token.
- One driver change (new call, new command). It must not weaken the receiver: the new call uses the same token check as the upload calls.
- Making Secrets from the console puts a token through the browser. It goes to the cluster's API server over TLS, as any other Secret made in the console does.
- The check Job adds a pod in `kube-system` for a few seconds.

## Decisions I made for you

- The check runs inside the source cluster, never in the browser.
- The check proves three things in order: the receiver answers, the certificate is trusted, the token is accepted. The health call alone proves only the first.
- A new, read-only receiver call is added for the token check, instead of trying a real upload.
- The console makes the Secrets (create only). It never reads one back.
- Create stays off until the check passes. An operator who wants to skip the check uses YAML.
- The destination cluster is not changed from the console. Its setup stays in the documents.
- The Secrets the console makes are named from the policy name and are deleted with the policy only if the operator agrees.
