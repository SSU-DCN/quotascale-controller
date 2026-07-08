echo "Creating namespaces"
oc new-project quotascale-controller

echo "Deploying QuotaScale Controller"
helm upgrade --install --force quotascale-controller ./helm-quotascale-controller

echo "Done"
