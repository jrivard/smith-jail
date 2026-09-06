# Copyright 2026 Jason D. Rivard <code@jrivard.org>
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# The per-session network proxy sidecar (cmd/smithjail-proxy). Built from
# this repo's own source at image-build time — see EnsureProxyImages in
# network.go — never published or fetched from a registry.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/smithjail-proxy ./cmd/smithjail-proxy
COPY internal/proxyproto ./internal/proxyproto
RUN CGO_ENABLED=0 go build -o /out/smithjail-proxy ./cmd/smithjail-proxy

# smithjailProxyUID must match the UID the netsetup helper's nft ruleset
# exempts from redirection (see network.go's nftRules) — otherwise this
# process's own outbound relay/upstream-DNS connections would be redirected
# straight back into itself.
FROM scratch
COPY --from=build /out/smithjail-proxy /smithjail-proxy
USER 5353:5353
ENTRYPOINT ["/smithjail-proxy"]
