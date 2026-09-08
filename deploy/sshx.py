#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""koolo-hub 远程部署辅助：paramiko 封装（密码/密钥均可）"""
import sys, os, io, stat
import paramiko

HOST = "8.133.228.22"
USER = "root"
PWD = os.environ.get("KOLOO_SSH_PW", "Dpsun+123")

def conn():
    c = paramiko.SSHClient()
    c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    c.connect(HOST, username=USER, password=PWD, timeout=25, banner_timeout=25,
              allow_agent=False, look_for_keys=False)
    return c

def run(cmd, timeout=180, show=True):
    c = conn()
    try:
        stdin, stdout, stderr = c.exec_command(cmd, timeout=timeout)
        out = stdout.read().decode("utf-8", "replace")
        err = stderr.read().decode("utf-8", "replace")
        rc = stdout.channel.recv_exit_status()
        if show:
            if out.strip():
                print(out, end="" if out.endswith("\n") else "\n")
            if err.strip():
                sys.stderr.write("[stderr] " + err + ("\n" if not err.endswith("\n") else ""))
            print("[rc=%d]" % rc)
        return rc, out, err
    finally:
        c.close()

def upload(local, remote):
    c = conn()
    try:
        sftp = c.open_sftp()
        sftp.put(local, remote)
        sftp.close()
        print("uploaded %s -> %s" % (local, remote))
    finally:
        c.close()

if __name__ == "__main__":
    args = sys.argv[1:]
    if not args:
        sys.exit("usage: sshx.py run <cmd> | put <local> <remote>")
    if args[0] == "run":
        run(" ".join(args[1:]))
    elif args[0] == "put":
        upload(args[1], args[2])
