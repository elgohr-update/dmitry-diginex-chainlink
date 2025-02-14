FROM smartcontract/builder:1.0.38

WORKDIR /chainlink
COPY go.mod go.sum yarn.lock package.json .yarnrc GNUmakefile ./
COPY tools/bin/ldflags tools/bin/ldflags
COPY tools/bin/restore-solc-cache tools/bin/restore-solc-cache
COPY .git .git
COPY VERSION VERSION
COPY .yarn .yarn
COPY patches patches
COPY solc_bin solc_bin
COPY belt/package.json belt/package.json
COPY belt/bin ./belt/bin
COPY evm-contracts/package.json evm-contracts/package.json
COPY evm-test-helpers/package.json evm-test-helpers/package.json
COPY integration/package.json integration/package.json
COPY integration-scripts/package.json integration-scripts/package.json
COPY operator_ui/package.json operator_ui/package.json
COPY tools/ci-ts/package.json tools/ci-ts/package.json
COPY tools/cypress-job-server/package.json tools/cypress-job-server/package.json
COPY tools/echo-server/package.json tools/echo-server/package.json
COPY tools/external-adapter/package.json tools/external-adapter/package.json
COPY tools/package.json tools/package.json

RUN make gen-builder-cache
