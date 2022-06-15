#!/bin/bash
set -eo pipefail

pachctl update pipeline -f src/server/cmd/sqlperftest/pipelines/ingress.json
pachctl update pipeline -f src/server/cmd/sqlperftest/pipelines/egress.json
pachctl update pipeline -f src/server/cmd/sqlperftest/pipelines/combine_ingress.json
pachctl update pipeline -f src/server/cmd/sqlperftest/pipelines/combine_egress.json
