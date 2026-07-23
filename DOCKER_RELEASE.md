# Backend Docker Image Release

This runbook publishes the customized backend image from this fork. Prefer GitHub Actions when possible; local Docker build remains a fallback. Run local commands from `backend/CLIProxyAPI`.

## Preferred Path: GitHub Actions

Workflow: `.github/workflows/docker-image.yml`

Trigger:

```bash
git tag v7.2.xx-gpt5.6
git push origin v7.2.xx-gpt5.6
```

Any tag matching `v*` starts the workflow. It builds `linux/amd64` + `linux/arm64`, then pushes multi-arch tags:

- `dx95/cliproxy:<tag>`
- `dx95/cliproxy:latest`

Required GitHub repository secrets on `starofkuku/CLIProxyAPI`:

- `DOCKERHUB_USERNAME`
- `DOCKERHUB_TOKEN`

Create the token on Docker Hub (Account Settings → Security → New Access Token) with read/write permission for `dx95/cliproxy`, then paste it into the GitHub secret. Do not commit the token.

## Local Fallback Target

- Docker Hub image: `dx95/cliproxy:gpt-5.6-v7.2.71`
- Local staging image: `cliproxyapi:local`
- Embedded version label: `gpt-5.6`
- Current local release platform: `linux/amd64`

The remote tag is intentionally reused for local fallback pushes. Pushing replaces the image referenced by that tag. Do not publish `latest`, change the tag, or push an image unless the user explicitly requested a Docker release.

## 1. Check the Source

Confirm that the intended backend changes are committed and pushed before building:

```bash
git status --short
git branch --show-current
git log -1 --format='%H %s'
go test ./...
go build -o /tmp/cli-proxy-api-check ./cmd/server
```

The normal fork branch is `main`, and `origin` should point to `starofkuku/CLIProxyAPI`. Do not include unrelated working-tree changes in a release. If SSH push fails, use GitHub CLI credentials over HTTPS:

```bash
git -c credential.https://github.com.helper='!gh auth git-credential' \
  push https://github.com/starofkuku/CLIProxyAPI.git main
```

## 2. Build the Image

The Dockerfile injects the source commit and UTC build time into the binary. Build the local staging tag first:

```bash
LOCAL_IMAGE=cliproxyapi:local
VERSION=gpt-5.6
COMMIT="$(git rev-parse --short HEAD)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

docker build \
  --tag "${LOCAL_IMAGE}" \
  --build-arg VERSION="${VERSION}" \
  --build-arg COMMIT="${COMMIT}" \
  --build-arg BUILD_DATE="${BUILD_DATE}" \
  --build-arg HTTP_PROXY \
  --build-arg HTTPS_PROXY \
  --build-arg ALL_PROXY \
  --build-arg NO_PROXY \
  .
```

Docker on the maintained build host already has an HTTP/HTTPS proxy configured. Check it with:

```bash
docker info 2>/dev/null | rg 'HTTP Proxy|HTTPS Proxy|No Proxy'
```

If another host needs a proxy, export its own `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and `NO_PROXY` values before building. Do not commit proxy addresses or credentials. The Dockerfile also defaults Go module downloads to `https://goproxy.cn,direct`; override with `--build-arg GOPROXY=...` only when required.

## 3. Verify the Local Image

```bash
docker image inspect cliproxyapi:local \
  --format 'id={{.Id}} platform={{.Os}}/{{.Architecture}} size={{.Size}}'

docker run --rm \
  --entrypoint /CLIProxyAPI/CLIProxyAPI \
  cliproxyapi:local --help 2>&1 | head -1
```

The second command must print a line containing the expected version, commit, and build time. If the platform is not `linux/amd64`, stop and confirm the requested target before pushing.

## 4. Tag and Push

Authenticate first if Docker Hub reports an authorization error:

```bash
docker login
```

Then replace the fixed release tag:

```bash
docker tag cliproxyapi:local dx95/cliproxy:gpt-5.6-v7.2.71
docker push dx95/cliproxy:gpt-5.6-v7.2.71
```

Record the `digest: sha256:...` printed by `docker push`.

## 5. Verify and Report

```bash
docker manifest inspect dx95/cliproxy:gpt-5.6-v7.2.71 >/dev/null
docker image inspect dx95/cliproxy:gpt-5.6-v7.2.71 \
  --format '{{range .RepoDigests}}{{println .}}{{end}}'
```

Report all of the following to the user:

- backend commit hash and subject;
- pushed tag `dx95/cliproxy:gpt-5.6-v7.2.71`;
- image platform (`linux/amd64` unless explicitly changed);
- pushed manifest digest;
- tests/build verification performed.

Do not rebuild the frontend or publish a frontend Release as part of this runbook unless the user separately requests it.
