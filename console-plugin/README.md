# OpenShift Console Plugin

OpenShift Console dynamic plugin that adds **Storage > IBM VPC File Pools** with dashboard, CRUD, and monitoring views.

> **Other docs you may need:**
>
> - [INSTALL.md](../INSTALL.md) — enabling, registering, and verifying the plugin on a cluster
> - [HELM-VALUES.md](../HELM-VALUES.md) — `consolePlugin.*` Helm values reference
> - [FEATURES.md](../FEATURES.md) — complete feature catalog
> - [UPGRADE-GUIDE.md](../UPGRADE-GUIDE.md) — version upgrade notes

---

## Architecture

The plugin uses [OpenShift Console's dynamic plugin framework](https://github.com/openshift/console/tree/master/frontend/packages/console-dynamic-plugin-sdk). Here's how the pieces fit together:

```
┌─────────────────────────────────────────────────────────────┐
│  Browser                                                    │
│                                                             │
│  ┌─────────────────────────────────────────────────┐        │
│  │  OpenShift Console (host app)                   │        │
│  │                                                 │        │
│  │  ┌───────────────────────────────────────┐      │        │
│  │  │  Plugin JS chunks (module federation) │      │        │
│  │  │  - VPCFilePoolsPage                   │      │        │
│  │  │  - Dashboard, List, Detail pages      │      │        │
│  │  └────────────────┬──────────────────────┘      │        │
│  │                   │ useK8sWatchResource          │        │
│  │                   │ consoleFetchJSON              │        │
│  └───────────────────┼─────────────────────────────┘        │
│                      │                                      │
└──────────────────────┼──────────────────────────────────────┘
                       │ /api/proxy/plugin/...
                       ▼
              ┌────────────────┐      ┌───────────────────┐
              │ Console Server │─────▶│ K8s API / Prom    │
              └────────┬───────┘      └───────────────────┘
                       │ proxy-fetch JS
                       ▼
              ┌────────────────┐
              │ nginx pod      │
              │ :9443 (TLS)    │
              │ serves dist/   │
              └────────────────┘
```

1. **Webpack module federation** (`ConsoleRemotePlugin`) builds the React code into JS chunks that the console host app can load at runtime.
2. An **nginx pod** serves those chunks over HTTPS (port 9443, TLS certificate from OpenShift's service-ca).
3. A **`ConsolePlugin` CR** tells the console where to fetch the JS — it points at the nginx Service.
4. **`console-extensions.json`** declares a nav item (`Storage > IBM VPC File Pools`) and a route (`/vpc-file-pools`) — the console loads the plugin code when the user navigates there.
5. Inside the browser, the plugin uses Console SDK hooks (`useK8sWatchResource`, `consoleFetchJSON`) to access the K8s API and Prometheus through the console's built-in proxy.

---

## Directory Structure

```
console-plugin/
├── src/
│   ├── index.ts                          # Plugin entry point (exposed module)
│   ├── constants.ts                      # API group, CRD versions, routes
│   ├── models.ts                         # K8s resource model definitions
│   ├── types.ts                          # TypeScript types for CRDs
│   ├── console-sdk-augments.d.ts         # Type augmentations for Console SDK
│   ├── components/
│   │   ├── VPCFilePoolsPage.tsx          # Top-level router (tab navigation)
│   │   ├── common/                       # Shared UI components
│   │   │   ├── CapacityBar.tsx           #   Capacity progress bar
│   │   │   ├── InlineCapacityBar.tsx     #   Inline capacity bar for tables
│   │   │   ├── ConditionsTable.tsx       #   CRD conditions display
│   │   │   ├── DeleteModal.tsx           #   Confirm-delete dialog
│   │   │   ├── FieldPopover.tsx          #   Field info popover
│   │   │   ├── HookEditor.tsx            #   Lifecycle hook editor
│   │   │   ├── PhaseStatus.tsx           #   Phase badge (Ready/Degraded/...)
│   │   │   ├── SchedulePresets.tsx       #   Cron schedule picker
│   │   │   └── YAMLEditorFallback.tsx    #   YAML editor wrapper
│   │   ├── dashboard/                    # Dashboard overview
│   │   │   ├── DashboardPage.tsx
│   │   │   └── CapacityDonut.tsx
│   │   ├── filesharepool/               # FileSharePool CRUD
│   │   │   ├── FileSharePoolListPage.tsx
│   │   │   ├── FileSharePoolDetailsPage.tsx
│   │   │   └── FileSharePoolCreatePage.tsx
│   │   ├── subvolume/                    # SubVolume CRUD
│   │   │   ├── SubVolumeListPage.tsx
│   │   │   ├── SubVolumeDetailsPage.tsx
│   │   │   └── SubVolumeCreatePage.tsx
│   │   ├── snapshot/                     # Snapshot CRUD
│   │   │   ├── SnapshotListPage.tsx
│   │   │   ├── SnapshotDetailsPage.tsx
│   │   │   └── SnapshotCreatePage.tsx
│   │   ├── volumegroupsnapshot/          # VolumeGroupSnapshot CRUD
│   │   │   ├── VolumeGroupSnapshotListPage.tsx
│   │   │   ├── VolumeGroupSnapshotDetailsPage.tsx
│   │   │   └── VolumeGroupSnapshotCreatePage.tsx
│   │   ├── replicationpolicy/            # ReplicationPolicy CRUD
│   │   │   ├── ReplicationPolicyListPage.tsx
│   │   │   ├── ReplicationPolicyDetailsPage.tsx
│   │   │   └── ReplicationPolicyCreatePage.tsx
│   │   └── monitoring/                   # Prometheus metrics views
│   │       ├── MonitoringPage.tsx
│   │       └── TimeSeriesChart.tsx
│   └── utils/
│       ├── resource-helpers.ts           # K8s object helpers
│       └── use-pool-metrics.ts           # Prometheus query hook
├── integration-tests/
│   ├── playwright.config.ts              # Playwright config
│   ├── auth.setup.ts                     # OCP login setup project
│   ├── support/
│   │   ├── constants.ts                  # Routes, selectors, column defs
│   │   └── pages.ts                      # Page object helpers
│   ├── tests/
│   │   ├── plugin.spec.ts               # Core plugin E2E tests
│   │   ├── golden-images.spec.ts         # Golden image syncer tests
│   │   └── pool-table-alignment.spec.ts  # Table layout tests
│   └── tsconfig.json
├── console-extensions.json               # Nav item + route declarations
├── package.json
├── tsconfig.json
├── webpack.config.ts
├── nginx.conf                            # NGINX config for prod container
├── Dockerfile                            # Multi-arch image
├── Dockerfile.amd64                      # AMD64-only image
└── start-console.sh                      # Local dev helper
```

---

## Local Development

### Prerequisites

- Node.js 18+, yarn 1.x
- `oc` CLI, logged into an OpenShift cluster (`oc login`)
- podman (to run the console bridge container)

### Steps

**Terminal 1** — start the webpack dev server:

```bash
cd console-plugin
yarn install
yarn dev          # webpack-dev-server on http://localhost:9001
```

**Terminal 2** — start the OpenShift console bridge:

```bash
cd console-plugin
./start-console.sh    # OCP console on http://localhost:9000
```

Open **http://localhost:9000**, navigate to **Storage > IBM VPC File Pools**.

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CONSOLE_IMAGE` | `quay.io/openshift/origin-console:latest` | Console bridge container image |
| `CONSOLE_PORT` | `9000` | Local port for the console UI |
| `PLUGIN_PORT` | `9001` | Port where webpack-dev-server runs |

---

## Build & Test

| Command | Description |
|---------|-------------|
| `yarn build` | Production build (output in `dist/`) |
| `yarn lint` | ESLint on `src/` |
| `yarn lint:fix` | ESLint with auto-fix |
| `yarn test` | Jest unit tests |
| `yarn test:watch` | Jest in watch mode |
| `yarn test:e2e` | Playwright E2E tests (requires running cluster) |
| `yarn test:e2e:report` | Open Playwright HTML report |
| `make console-plugin-docker-build` | Build the container image (from repo root) |

---

## Deployment

The Helm chart creates four resources when `consolePlugin.enabled: true`:

| Template | Resource | Purpose |
|----------|----------|---------|
| `console-plugin-configmap.yaml` | ConfigMap | nginx.conf (listens on port 9443 with TLS) |
| `console-plugin-deployment.yaml` | Deployment | nginx container serving `dist/` assets |
| `console-plugin-service.yaml` | Service | ClusterIP service with `serving-cert-secret-name` annotation |
| `console-plugin-cr.yaml` | ConsolePlugin | Registers the plugin with OpenShift Console |

**TLS:** The Service's `service.beta.openshift.io/serving-cert-secret-name` annotation causes OpenShift's service-ca operator to generate a TLS Secret. The Deployment mounts this Secret at `/var/cert/` for nginx.

---

## Key Technologies

| Technology | Version |
|------------|---------|
| React | 18 |
| PatternFly | 6 |
| OpenShift Console SDK | 4.20.0 |
| TypeScript | 5.7 |
| Webpack | 5 |
| Playwright | 1.50 |
