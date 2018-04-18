#!/bin/bash
# This script pushes docker images to the minikube vm so that they can be
# pulled/run by kubernetes pods

if [[ $# -ne 1 ]]; then
  echo "error: need the name of the docker image to push"
fi

# Detect if minikube was started with --vm-driver=none by inspecting the output
# from 'minikube docker-env'
if minikube docker-env \
    | grep -q "'none' driver does not support 'minikube docker-env' command"
then
  exit 0 # Nothing to push -- vm-driver=none uses the system docker daemon
fi

docker save "${1}" | pv | (
  eval $(minikube docker-env)
  docker load
)
