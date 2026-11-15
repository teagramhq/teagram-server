#!/usr/bin/env python3
"""Render the fixed QA bot job using the currently running server image."""

import json
import pathlib
import re
import sys


def main():
    if len(sys.argv) != 3:
        raise SystemExit("usage: render-qa-bot-job.py <server-image> <operation>")
    image, operation = sys.argv[1:]
    if not re.fullmatch(r"ghcr\.io/teagramhq/teagramd(?:@sha256:[0-9a-f]{64}|:ci)", image):
        raise SystemExit("invalid teagramd image")
    if operation not in {"create", "rotate", "revoke", "status", "probe"}:
        raise SystemExit("invalid QA bot operation")

    script = pathlib.Path(__file__).with_name("qa-bot-operation.sh").read_text()
    restricted = {
        "allowPrivilegeEscalation": False,
        "readOnlyRootFilesystem": True,
        "capabilities": {"drop": ["ALL"]},
    }
    job = {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {"name": "qa-bot-operation", "namespace": "teagram"},
        "spec": {
            "backoffLimit": 0,
            "activeDeadlineSeconds": 180,
            "ttlSecondsAfterFinished": 360,
            "template": {
                "metadata": {"labels": {"app.kubernetes.io/name": "qa-bot-operation"}},
                "spec": {
                    "restartPolicy": "Never",
                    "automountServiceAccountToken": False,
                    "securityContext": {"seccompProfile": {"type": "RuntimeDefault"}},
                    "initContainers": [{
                        "name": "prepare-shell",
                        "image": "mirror.gcr.io/library/busybox:1.37.0-uclibc@sha256:7aa3d3c53cc1ed823c6a5967ebaac87700a5b0766af45ff599dbca3506c8b072",
                        "command": ["sh", "-ec", "cp /bin/busybox /tools/busybox; /tools/busybox --install -s /tools; chmod 0750 /key-copy; chown 65532:65532 /key-copy"],
                        "securityContext": {**restricted, "runAsUser": 0, "capabilities": {"drop": ["ALL"], "add": ["CHOWN"]}},
                        "resources": {"requests": {"cpu": "10m", "memory": "16Mi"}, "limits": {"cpu": "100m", "memory": "64Mi"}},
                        "volumeMounts": [{"name": "tools", "mountPath": "/tools"}, {"name": "key-copy", "mountPath": "/key-copy"}],
                    }, {
                        "name": "copy-auth-key",
                        "image": "mirror.gcr.io/library/busybox:1.37.0-uclibc@sha256:7aa3d3c53cc1ed823c6a5967ebaac87700a5b0766af45ff599dbca3506c8b072",
                        "command": ["sh", "-ec", "test -f /source/enc_key.hex && test ! -L /source/enc_key.hex; umask 077; cp /source/enc_key.hex /key-copy/enc_key.hex; chmod 0640 /key-copy/enc_key.hex"],
                        "securityContext": {**restricted, "runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532},
                        "resources": {"requests": {"cpu": "10m", "memory": "16Mi"}, "limits": {"cpu": "100m", "memory": "64Mi"}},
                        "volumeMounts": [
                            {"name": "key-data", "mountPath": "/source", "readOnly": True},
                            {"name": "key-copy", "mountPath": "/key-copy"},
                        ],
                    }],
                    "containers": [{
                        "name": "qa-bot",
                        "image": image,
                        "imagePullPolicy": "IfNotPresent",
                        "command": ["/tools/busybox", "sh", "-c", script, "qa-bot-operation", operation],
                        "env": [
                            {"name": "PATH", "value": "/tools"},
                            {"name": "HOME", "value": "/tmp"},
                            {"name": "TG_AUTHKEY_ENC_KEY_FILE", "value": "/run/qa-bot/enc_key.hex"},
                            {"name": "TG_POSTGRES_DSN", "valueFrom": {"secretKeyRef": {"name": "postgres-credentials", "key": "TG_POSTGRES_DSN"}}},
                        ],
                        "securityContext": {**restricted, "runAsUser": 0, "runAsGroup": 65532},
                        "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}, "limits": {"cpu": "1", "memory": "512Mi"}},
                        "volumeMounts": [
                            {"name": "tools", "mountPath": "/tools", "readOnly": True},
                            {"name": "key-copy", "mountPath": "/run/qa-bot", "readOnly": True},
                            {"name": "tmp", "mountPath": "/tmp"},
                        ],
                    }],
                    "volumes": [
                        {"name": "tools", "emptyDir": {}},
                        {"name": "key-data", "persistentVolumeClaim": {"claimName": "key-data-teagramd-0", "readOnly": True}},
                        {"name": "key-copy", "emptyDir": {"medium": "Memory", "sizeLimit": "1Mi"}},
                        {"name": "tmp", "emptyDir": {}},
                    ],
                },
            },
        },
    }
    json.dump(job, sys.stdout, separators=(",", ":"))
    print()


if __name__ == "__main__":
    main()
