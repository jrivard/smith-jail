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

# One-shot helper: joins a session's network namespace (via
# `--network container:<proxy>`) with --cap-add=NET_ADMIN, applies the
# ruleset it's handed on stdin, then exits. Nothing long-lived ever holds
# NET_ADMIN — see network.go for how smith-jail runs this.
FROM alpine:3.20
RUN apk add --no-cache nftables
ENTRYPOINT ["nft", "-f", "-"]
