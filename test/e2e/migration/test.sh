#!/bin/bash -ex
#
# Copyright (c) 2023 Red Hat, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# get the last image tag from quay.io
img_repo_api="https://quay.io/api/v1/repository/redhat-user-workloads/maestro-rhtap-tenant/maestro/maestro"
img_registry="quay.io/redhat-user-workloads/maestro-rhtap-tenant"
last_tag=$(curl -s -X GET "${img_repo_api}" | jq -s -c -r 'sort_by(.tags[].last_modified) | .[].tags[].name' | grep -E '^[a-z0-9]{40}$' | head -n 1)

# run the e2e-test with the last published image
# TODO use e2e test env variable `e2e_test_label_filer` to pick up the test cases to avoid
# compatibility problem, e.g. if one new feature is added, the new e2e will not be able to
# run with the last image.
image_tag=$last_tag external_image_registry=$img_registry internal_image_registry=$img_registry make e2e-test

# build image with the latest code
export namespace="maestro"
export image_tag="latest"
export external_image_registry="image-registry.testing"
export internal_image_registry="image-registry.testing"

latest_img="${external_image_registry}/${namespace}/maestro:${image_tag}"

make image
# related issue: https://github.com/kubernetes-sigs/kind/issues/2038
if command -v docker &> /dev/null; then
    kind load docker-image $latest_img --name maestro
elif command -v podman &> /dev/null; then
    podman save $latest_img -o /tmp/maestro.tar 
    kind load image-archive /tmp/maestro.tar --name maestro 
    rm /tmp/maestro.tar
else
    echo "Neither Docker nor Podman is installed, exiting"
    exit 1
fi

# update the last image with the latest image
export KUBECONFIG=${PWD}/test/e2e/.kubeconfig
kubectl -n $namespace set image deployment/maestro migration=$latest_img service=$latest_img
kubectl wait deploy/maestro -n $namespace --for condition=Available=True --timeout=200s

sleep 5

# run the e2e test with the latest image
make e2e-test/run
