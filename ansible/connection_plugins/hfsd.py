# Ansible connection plugin that reaches the Space through hfsd's remote API.
#
# The protocol lives in the `hfs` binary; this shells out to it the same way
# the stock ssh plugin shells out to ssh. `hfs` sets HFS_BIN and HFS_CONFIG
# when it launches ansible-playbook.

DOCUMENTATION = """
name: hfsd
short_description: Run tasks on a Hugging Face Space through hfsd
description:
  - Executes commands and transfers files over hfsd's websocket/HTTP API
    by calling the C(hfs) command line tool.
author: aldehir
options:
  hfs_bin:
    description: Path to the hfs binary.
    default: hfs
    env:
      - name: HFS_BIN
    vars:
      - name: ansible_hfs_bin
"""

import subprocess

from ansible.errors import AnsibleConnectionFailure, AnsibleError
from ansible.plugins.connection import ConnectionBase
from ansible.utils.display import Display

display = Display()

# `hfs` exits with this when it couldn't reach hfsd at all, as opposed to the
# remote command failing.
EXIT_UNREACHABLE = 255


class Connection(ConnectionBase):
    transport = "hfsd"
    has_pipelining = True

    def _connect(self):
        self._connected = True
        return self

    def _hfs(self, *args, in_data=None):
        cmd = [self.get_option("hfs_bin"), *args]
        display.vvv("EXEC %s" % " ".join(cmd), host=self._play_context.remote_addr)
        p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        stdout, stderr = p.communicate(in_data)
        if p.returncode == EXIT_UNREACHABLE:
            raise AnsibleConnectionFailure(stderr.decode(errors="replace").strip())
        return p.returncode, stdout, stderr

    def exec_command(self, cmd, in_data=None, sudoable=True):
        super().exec_command(cmd, in_data=in_data, sudoable=sudoable)
        return self._hfs("exec", "--shell", cmd, in_data=in_data)

    def put_file(self, in_path, out_path):
        super().put_file(in_path, out_path)
        rc, _, stderr = self._hfs("put", in_path, out_path)
        if rc != 0:
            raise AnsibleError("failed to put %s: %s" % (out_path, stderr.decode(errors="replace").strip()))

    def fetch_file(self, in_path, out_path):
        super().fetch_file(in_path, out_path)
        rc, _, stderr = self._hfs("fetch", in_path, out_path)
        if rc != 0:
            raise AnsibleError("failed to fetch %s: %s" % (in_path, stderr.decode(errors="replace").strip()))

    def close(self):
        self._connected = False
