# Helm Chart OCI Proxy

`helm-charts-oci-proxy` exposes classic Helm chart repositories through an OCI registry interface.

It lets you pull charts like this:

```bash
helm pull oci://your-proxy.example.com/charts.jetstack.io/cert-manager --version 1.11.2
```

instead of consuming the upstream chart repository directly.

## Project Goal

This repository is intentionally focused on three things:

- building the application as a container image
- running it as a Dockerized service
- deploying it to Kubernetes with the included Helm chart

The proxy fetches `index.yaml` and chart archives from upstream Helm repositories, converts them into OCI artifacts on demand, and serves them through registry-style endpoints.

## How It Works

When a client requests a chart such as:

```bash
helm pull oci://your-proxy.example.com/charts.bitnami.com/bitnami/redis --version 20.6.0
```

the proxy will:

1. download the upstream Helm repository index
2. resolve the requested chart version
3. fetch the chart archive
4. convert it into OCI manifest + blobs
5. return it through the OCI registry API

## Run With Docker

Build the image:

```bash
docker build -t helm-charts-oci-proxy .
```

Run the proxy:

```bash
docker run --rm -p 9000:9000 helm-charts-oci-proxy
```

Then pull through it:

```bash
helm pull oci://localhost:9000/charts.jetstack.io/cert-manager --version 1.11.2
```

## Deploy With Helm

Install the included chart from this repository:

```bash
helm install chartproxy ./chart --create-namespace --namespace chartproxy
```

Or render it first:

```bash
helm template chartproxy ./chart
```

By default the chart deploys the proxy as a single pod with in-memory storage.

## Release Model

The repository uses a simple deployment-focused versioning flow:

- pushes to `main` run CI validation (`go test`, `helm lint`, `helm template`), but do not publish release artifacts
- pushing a git tag like `v1.2.3` publishes a Docker image tagged `1.2.3`
- the same git tag also publishes the Helm chart with `version: 1.2.3` and `appVersion: 1.2.3`

This keeps Docker and Helm releases aligned and ensures artifacts are published only for explicit tagged releases.

## Configuration

Main environment variables:

- `PORT` - listen port, default `9000`
- `DEBUG` - enable debug logging
- `MANIFEST_CACHE_TTL` - manifest/blob cache TTL in seconds, default `60`
- `INDEX_CACHE_TTL` - upstream index cache TTL in seconds, default `14400`
- `INDEX_ERROR_CACHE_TTL` - retry delay after index fetch failure in seconds, default `30`
- `DOWNLOAD_TIMEOUT` - upstream HTTP timeout in seconds, default `30`
- `MAX_INDEX_BYTES` - maximum accepted upstream `index.yaml` size, default `33554432`
- `MAX_CHART_BYTES` - maximum accepted upstream chart archive size, default `268435456`
- `USE_TLS` - enable direct TLS serving inside the container
- `CERT_FILE` - TLS certificate path, default `/tls/tls.crt`
- `KEY_FILE` - TLS key path, default `/tls/tls.key`
- `REWRITE_DEPENDENCIES` - rewrite chart dependency repositories to point back to this proxy
- `PROXY_HOST` - required when `REWRITE_DEPENDENCIES=true`

For Kubernetes, TLS is usually terminated by an Ingress or reverse proxy. If you want the application itself to serve TLS, mount a secret at `/tls` or override `CERT_FILE` and `KEY_FILE`.

## Dependency Rewriting

If `REWRITE_DEPENDENCIES=true`, dependency repositories inside `Chart.yaml` are rewritten from URLs like:

```yaml
repository: https://charts.bitnami.com/bitnami
```

to:

```yaml
repository: oci://your-proxy.example.com/charts.bitnami.com/bitnami
```

This is useful when you want dependency resolution to stay inside your proxy path as well.

Note that rewriting changes the chart archive contents, so Helm provenance/signature verification will no longer match the original upstream package.

## License and Origin

This project is licensed under the GNU Affero General Public License v3.0. See `LICENSE`.

It was originally developed at `https://github.com/container-registry/helm-charts-oci-proxy`, and that origin should remain credited in derivative and redistributed versions of the project.
