#!/usr/bin/env python3
"""Exercise web deployer RBAC and admission in a disposable Kubernetes cluster."""
import copy
import hashlib
import json
import os
import subprocess

SA = "system:serviceaccount:teagram:teagram-web-deployer"
NAMESPACE = "teagram"


def kubectl(*args, data=None, ok=True):
    result = subprocess.run(["kubectl", *args], input=data, text=True, capture_output=True)
    if (result.returncode == 0) != ok:
        raise AssertionError(f"kubectl {args}: {result.stdout}{result.stderr}")
    return result.stdout + result.stderr


def apply(obj):
    kubectl("apply", "-n", NAMESPACE, "-f", "-", data=json.dumps(obj))


def patch(kind, changes, expected=True, policy=True):
    output = kubectl("--as", SA, "-n", NAMESPACE, "patch", kind, "teagram-web",
                     "--type=merge", "--dry-run=server", "-p", json.dumps(changes), ok=expected)
    if not expected and policy and "ValidatingAdmissionPolicy 'teagram-web-deployer'" not in output:
        raise AssertionError(output)


def can_i(expected, verb, resource, name=None, subresource=None):
    args = ["auth", "can-i", verb, f"{resource}/{name}" if name else resource]
    if subresource:
        args += ["--subresource", subresource]
    args += ["--as", SA, "-n", NAMESPACE]
    result = subprocess.run(["kubectl", *args], text=True, capture_output=True)
    answer = result.stdout.strip()
    if answer != expected or result.returncode != (0 if expected == "yes" else 1):
        raise AssertionError(f"{args}: {answer} {result.stderr}")
    print(f"{verb} {resource}/{name or '*'}: {answer}")


def template(**kwargs):
    return {"spec": {"template": {"spec": kwargs}}}


def container(**kwargs):
    obj = copy.deepcopy(web_container)
    obj.update(kwargs)
    return template(containers=[obj])


labels = {"app.kubernetes.io/name": "teagram-web"}
web_container = {
    "name": "teagram-web",
    "image": "ghcr.io/teagramhq/teagram-web@sha256:" + "a" * 64,
    "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True},
    "resources": {"limits": {"cpu": "500m", "memory": "256Mi"}},
}
deployment = {
    "apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "teagram-web"},
    "spec": {"replicas": 0, "selector": {"matchLabels": labels},
             "template": {"metadata": {"labels": labels}, "spec": {
                 "automountServiceAccountToken": False, "containers": [web_container],
                 "volumes": [{"name": "tmp", "emptyDir": {"sizeLimit": "16Mi"}}],
             }}},
}
service = {
    "apiVersion": "v1", "kind": "Service", "metadata": {"name": "teagram-web"},
    "spec": {"type": "ClusterIP", "selector": labels, "ports": [{"port": 80, "targetPort": 8080}]},
}
if os.environ.get("WEB_DEPLOYER_TEST_DISPOSABLE") != "1":
    raise SystemExit("WEB_DEPLOYER_TEST_DISPOSABLE=1 is required for this destructive cluster test")
existing = kubectl("get", "deployment/teagram-web", "service/teagram-web", "-n", NAMESPACE,
                   "--ignore-not-found", "-o", "name")
if existing.strip():
    raise SystemExit("test requires a disposable cluster without teagram-web resources")
probe_names = ["teagram-web-network-probe", "teagram-web-linkselector-probe",
               "teagram-web-cloudflared-probe", "teagram-web-unrelated-probe"]
existing_probes = kubectl("get", *(f"pod/{name}" for name in probe_names), "-n", NAMESPACE,
                          "--ignore-not-found", "-o", "name")
if existing_probes.strip():
    raise SystemExit("test requires a disposable cluster without web network probe pods")
apply(deployment)
apply(service)
role = json.loads(kubectl("get", "role", "teagram-web-deployer", "-n", NAMESPACE, "-o", "json"))
expected_rules = [
    {"apiGroups": ["apps"], "resources": ["deployments"], "resourceNames": ["teagram-web"], "verbs": ["get", "list", "watch", "patch"]},
    {"apiGroups": ["apps"], "resources": ["deployments"], "resourceNames": ["cloudflared"], "verbs": ["get"]},
    {"apiGroups": [""], "resources": ["services"], "resourceNames": ["teagram-web"], "verbs": ["get", "patch"]},
    {"apiGroups": [""], "resources": ["services/proxy"], "resourceNames": ["teagram-web"], "verbs": ["get"]},
    {"apiGroups": ["apps"], "resources": ["replicasets"], "verbs": ["get", "list"]},
    {"apiGroups": [""], "resources": ["pods", "events"], "verbs": ["list"]},
]
assert role["rules"] == expected_rules, role["rules"]
account = json.loads(kubectl("get", "serviceaccount", "teagram-web-deployer", "-n", NAMESPACE, "-o", "json"))
assert account["automountServiceAccountToken"] is False
role_binding = json.loads(kubectl("get", "rolebinding", "teagram-web-deployer", "-n", NAMESPACE, "-o", "json"))
assert role_binding["subjects"] == [{"kind": "ServiceAccount", "name": "teagram-web-deployer", "namespace": NAMESPACE}]
assert role_binding["roleRef"] == {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "teagram-web-deployer"}
policy = json.loads(kubectl("get", "validatingadmissionpolicy", "teagram-web-deployer", "-o", "json"))
assert policy["spec"]["failurePolicy"] == "Fail"
assert len(policy["spec"]["matchConditions"]) == 1
assert "request.namespace == 'teagram'" in policy["spec"]["matchConditions"][0]["expression"]
assert f"request.userInfo.username == '{SA}'" in policy["spec"]["matchConditions"][0]["expression"]
# Lock the admission contract, including fields Kubernetes rejects before admission.
contract = {key: policy["spec"][key] for key in ("matchConstraints", "matchConditions", "validations")}
digest = hashlib.sha256(json.dumps(contract, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
assert digest == "21b1c1f7eb197a8d5408b0cae83e6d281af911ca567bcb0e023aedc86fd2dd61", digest
binding = json.loads(kubectl("get", "validatingadmissionpolicybinding", "teagram-web-deployer", "-o", "json"))
assert binding["spec"]["policyName"] == "teagram-web-deployer"
assert binding["spec"]["validationActions"] == ["Deny"]
assert binding["spec"]["matchResources"]["namespaceSelector"]["matchLabels"] == {"kubernetes.io/metadata.name": NAMESPACE}
network = json.loads(kubectl("get", "networkpolicy", "teagram-web", "-n", NAMESPACE, "-o", "json"))["spec"]
network.setdefault("egress", [])
assert network == {
    "podSelector": {"matchLabels": labels},
    "policyTypes": ["Ingress", "Egress"],
    "ingress": [{"from": [
                    {"podSelector": {"matchLabels": {"app.kubernetes.io/name": "cloudflared"}}},
                    {"podSelector": {"matchLabels": {"app.kubernetes.io/name": "linkselector"}}},
                 ],
                 "ports": [{"protocol": "TCP", "port": 8080}]}],
    "egress": [],
}, network

probe_pods = [
    {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": probe_names[0], "labels": labels},
     "spec": {"automountServiceAccountToken": False, "containers": [{
         "name": "web", "image": "busybox:1.36",
         "command": ["sh", "-c", "mkdir -p /tmp/www && printf web-ok > /tmp/www/healthz && httpd -f -p 8080 -h /tmp/www"],
         "readinessProbe": {"httpGet": {"path": "/healthz", "port": 8080}, "periodSeconds": 2},
     }]}},
    {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": probe_names[1], "labels": {"app.kubernetes.io/name": "linkselector"}},
     "spec": {"automountServiceAccountToken": False, "containers": [{"name": "client", "image": "busybox:1.36", "command": ["sleep", "3600"]}]}},
    {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": probe_names[2], "labels": {"app.kubernetes.io/name": "cloudflared"}},
     "spec": {"automountServiceAccountToken": False, "containers": [{"name": "client", "image": "busybox:1.36", "command": ["sleep", "3600"]}]}},
    {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": probe_names[3], "labels": {"app.kubernetes.io/name": "unrelated"}},
     "spec": {"automountServiceAccountToken": False, "containers": [{"name": "client", "image": "busybox:1.36", "command": ["sleep", "3600"]}]}},
]
for probe_pod in probe_pods:
    apply(probe_pod)
kubectl("wait", "--for=condition=Ready", "--timeout=120s", "-n", NAMESPACE,
        *(f"pod/{name}" for name in probe_names))
web_pod = json.loads(kubectl("get", f"pod/{probe_names[0]}", "-n", NAMESPACE, "-o", "json"))
web_ip = web_pod["status"]["podIP"]
if not web_ip:
    raise AssertionError("web network probe pod has no IP")
proxy_health = kubectl(
    "--as", SA, "get", "--raw",
    f"/api/v1/namespaces/{NAMESPACE}/services/teagram-web/proxy/healthz",
)
if proxy_health.strip() != "web-ok":
    raise AssertionError(f"the web deployer service proxy returned an unexpected response: {proxy_health!r}")
print("web deployer reached the fallback through the service proxy")
for name in probe_names[1:3]:
    result = kubectl("exec", "-n", NAMESPACE, f"pod/{name}", "--", "wget", "-qO-", "-T", "3",
                     f"http://{web_ip}:8080/healthz")
    if result.strip() != "web-ok":
        raise AssertionError(f"{name} received an unexpected web probe response: {result!r}")
    print(f"{name} reached web network probe")
kubectl("exec", "-n", NAMESPACE, f"pod/{probe_names[3]}", "--", "wget", "-qO-", "-T", "3",
        f"http://{web_ip}:8080/healthz", ok=False)
print("unrelated pod was blocked from web network probe")
kubectl("delete", "-n", NAMESPACE, "--wait=true", *(f"pod/{name}" for name in probe_names))
for verb, resource, name in [("patch", "deployments.apps", "teagram-web"),
                             ("get", "deployments.apps", "teagram-web"),
                             ("patch", "services", "teagram-web"),
                             ("get", "services", "teagram-web"),
                             ("get", "deployments.apps", "cloudflared"),
                             ("get", "replicasets.apps", "arbitrary"),
                             ("list", "replicasets.apps", None), ("list", "pods", None),
                             ("list", "events", None)]:
    can_i("yes", verb, resource, name)
for verb, resource, name in [("patch", "statefulsets.apps", "teagramd"),
                             ("patch", "statefulsets.apps", "postgres"),
                             ("patch", "deployments.apps", "cloudflared"),
                             ("create", "jobs.batch", None), ("create", "pods", None),
                             ("get", "secrets", None), ("get", "pods/log", None),
                             ("create", "pods/exec", None), ("patch", "networkpolicies", "teagram-web")]:
    can_i("no", verb, resource, name)
can_i("yes", "get", "services", "teagram-web", subresource="proxy")
can_i("no", "get", "services", "kubernetes", subresource="proxy")
can_i("no", "get", "pods")
can_i("no", "create", "pods", subresource="portforward")

patch("deployment", {"metadata": {"annotations": {"validation-check": "ok"}}})
patch("service", {"metadata": {"annotations": {"validation-check": "ok"}}})
patch("deployment", {"spec": {"replicas": 2}})
patch("deployment", container(resources={"limits": {"cpu": "0.5", "memory": "268435456"}}))
patch("deployment", template(volumes=[{"name": "tmp", "emptyDir": {"sizeLimit": "16777216"}}]))
checks = {
    "secret volume": template(volumes=[{"name": "secret", "secret": {"secretName": "other"}}]),
    "foreign service account": template(serviceAccountName="teagram-deployer"),
    "token mount": template(automountServiceAccountToken=True),
    "additional label": {"spec": {"template": {"metadata": {"labels": {"other": "value"}}}}},
    "untrusted image": container(image="example.invalid/other:latest"),
    "container name": container(name="other"),
    "sidecar": template(containers=[web_container, {"name": "other", "image": "busybox"}]),
    "environment": container(env=[{"name": "TEST", "value": "test"}]),
    "environment source": container(envFrom=[{"secretRef": {"name": "other"}}]),
    "init container": template(initContainers=[{"name": "other", "image": "busybox"}]),
    "image pull secret": template(imagePullSecrets=[{"name": "other"}]),
    "privilege escalation": container(securityContext={"allowPrivilegeEscalation": True, "readOnlyRootFilesystem": True}),
    "writable root filesystem": container(securityContext={"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": False}),
    "missing read-only root filesystem": container(securityContext={"allowPrivilegeEscalation": False}),
    "probe host": container(livenessProbe={"httpGet": {"host": "teagramd", "port": 2443}}),
    "lifecycle hook": container(lifecycle={"postStart": {"exec": {"command": ["true"]}}}),
    "critical priority": template(priorityClassName="system-node-critical"),
    "too many replicas": {"spec": {"replicas": 3}},
    "pod-level resources": template(resources={"limits": {"cpu": "1", "memory": "1Gi"}}),
    "missing cpu limit": container(resources={"limits": {"memory": "256Mi"}}),
    "missing memory limit": container(resources={"limits": {"cpu": "500m"}}),
    "zero cpu limit": container(resources={"limits": {"cpu": "0", "memory": "256Mi"}}),
    "zero memory limit": container(resources={"limits": {"cpu": "500m", "memory": "0"}}),
    "oversized cpu limit": container(resources={"limits": {"cpu": "501m", "memory": "256Mi"}}),
    "oversized memory limit": container(resources={"limits": {"cpu": "500m", "memory": "257Mi"}}),
    "emptyDir without size limit": template(volumes=[{"name": "tmp", "emptyDir": {}}]),
    "zero emptyDir size limit": template(volumes=[{"name": "tmp", "emptyDir": {"sizeLimit": "0"}}]),
    "oversized emptyDir": template(volumes=[{"name": "tmp", "emptyDir": {"sizeLimit": "17Mi"}}]),
}
for name, changes in checks.items():
    patch("deployment", changes, expected=False)
    print(f"rejected {name}")
patch("deployment", {"spec": {"template": {"metadata": {"labels": {"app.kubernetes.io/name": "teagramd"}}}}}, expected=False, policy=False)
for name, changes in {
    "NodePort": {"spec": {"type": "NodePort"}},
    "foreign selector": {"spec": {"selector": {"app.kubernetes.io/name": "teagramd"}}},
    "external IP": {"spec": {"externalIPs": ["192.0.2.1"]}},
}.items():
    patch("service", changes, expected=False)
    print(f"rejected {name}")
print("web deployer checks passed")
