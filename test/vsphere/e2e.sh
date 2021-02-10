#!/bin/bash

mkdir -p .bin
mkdir -p .certs

export TERM=xterm-256color
REPO=$(basename $(git remote get-url origin | sed 's/\.git//'))
BIN=.bin/karina
chmod +x $BIN

generate_cluster_id() {
  echo e2e-$(date "+%d%H%M")
}

export PLATFORM_CLUSTER_ID=$(generate_cluster_id)
export PLATFORM_OPTIONS_FLAGS="-e name=${PLATFORM_CLUSTER_ID} -e domain=${PLATFORM_CLUSTER_ID}.lab.flanksource.com -v"
export PLATFORM_CONFIG=${PLATFORM_CONFIG:-test/vsphere/vsphere.yaml}
unset KUBECONFIG

mkdir -p ~/.ssh
chmod 700 ~/.ssh
echo "$SSH_SECRET_KEY_BASE64" | base64 -d > ~/.ssh/id_rsa
chmod 600 ~/.ssh/id_rsa

sshuttle --dns -e "ssh -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no" -r $SSH_USER@$SSH_JUMP_HOST $VPN_NETWORK &
SSHUTTLE_PID=$?
# Wait for connection
sleep 2s

printf "\n\n\n\n$(tput bold)Generate Certs$(tput setaf 7)\n"
$BIN ca generate --name root-ca --cert-path .certs/root-ca.crt --private-key-path .certs/root-ca.key --password foobar  --expiry 1
$BIN ca generate --name ingress-ca --cert-path .certs/ingress-ca.crt --private-key-path .certs/ingress-ca.key --password foobar  --expiry 1
$BIN ca generate --name sealed-secrets --cert-path .certs/sealed-secrets-crt.pem --private-key-path .certs/sealed-secrets-key.pem --password foobar  --expiry 1

printf "\n\n\n\n$(tput bold)Provision Cluster$(tput setaf 7)\n"
$BIN provision vsphere-cluster $PLATFORM_OPTIONS_FLAGS

printf "\n\n\n\n$(tput bold)Basic Deployments$(tput setaf 7)\n"

$BIN deploy phases --crds --calico --base --stubs --dex $PLATFORM_OPTIONS_FLAGS

failed=false

printf "\n\n\n\n$(tput bold)Up?$(tput setaf 7)\n"
# wait for the base deployment with stubs to come up healthy
if ! $BIN test phases --base --stubs --wait 120 --progress=false $PLATFORM_OPTIONS_FLAGS; then
  echo "Failed setting up the basic deployment"
  failed=true
fi

if [[ "$failed" = false ]]; then
  printf "\n\n\n\n$(tput bold)All Deployments$(tput setaf 7)\n"
  $BIN deploy all $PLATFORM_OPTIONS_FLAGS

  ## e2e do not use --wait at the run level, if needed each individual test implements
  ## its own wait. e2e tests should always pass once the non e2e have passed
  if ! $BIN test  all --e2e --progress=false --junit-path test-results/results.xml $PLATFORM_OPTIONS_FLAGS; then
    echo "Failure in feature test"
    failed=true
  fi
fi

printf "\n\n\n\n$(tput bold)Reporting$(tput setaf 7)\n"
wget -nv https://github.com/flanksource/build-tools/releases/download/v0.11.2/build-tools
chmod +x build-tools
./build-tools gh actions report-junit test-results/results.xml --token $GIT_API_KEY --build "$BUILD"

mkdir -p artifacts
$BIN snapshot --output-dir snapshot -v --include-specs=true --include-logs=true --include-events=true $PLATFORM_OPTIONS_FLAGS
zip -r artifacts/snapshot.zip snapshot/*

$BIN terminate-orphans $PLATFORM_OPTIONS_FLAGS || echo "Orphans not terminated."
$BIN cleanup $PLATFORM_OPTIONS_FLAGS
#kill "$SSHUTTLE_PID"

if [[ "$failed" = true ]]; then
  echo "Test failed."
  exit 1
fi
echo "Test passed!"
