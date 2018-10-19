#!/usr/bin/env bash

AWS_PROFILE=${1:-default}
SECRET_NAME=aws-creds

SECRET_EXISTS=`kubectl get secrets | grep -q $SECRET_NAME`

if kubectl get secrets | grep -q $SECRET_NAME; then
    echo "Deleting secret $SECRET_NAME"
    kubectl delete secret $SECRET_NAME
else
    echo "Secret $SECRET_NAME does not exist yet"
fi

echo "Fetching fresh ECR token using profile $AWS_PROFILE"
TOKEN=`aws ecr get-login --region eu-central-1 --profile $AWS_PROFILE --no-include-email | awk '{ print $6 }'`

echo "(Re)creating secret aws_cred"
kubectl create secret docker-registry $SECRET_NAME --docker-server=460465941927.dkr.ecr.eu-central-1.amazonaws.com --docker-username=AWS --docker-password=$TOKEN --docker-email=ctt@wgmobile.net
