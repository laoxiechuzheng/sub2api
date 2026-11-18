"""准确 d78/候选完整 Dockerfile + normal bootstrap 的隔离 CI 探针。

只在 Linux Actions 显式执行；不连接外部 DSN、不读取生产配置、不发布原始
app 日志/Env/认证响应。A–F 与真实 invalid-index recovery 都由实际运行后置真。
"""
import argparse
import base64
import datetime as dt
import hashlib
import hmac
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request

_spec = importlib.util.spec_from_file_location('v0222_evidence', Path(__file__).with_name('v0222_evidence.py'))
evidence = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(evidence)
OLD_SHA = 'd78de0d0edab4731c853ac9e94c0de7cede055c6'
OFFICIAL_SHA = 'b0fa9f3e6eeab1968a53e1289b69daa4bb40ca2b'
OLD_VERSION = '0.2.20-custom-20261109'
OCI_SOURCE = 'https://github.com/laoxiechuzheng/sub2api'
PG_IMAGE, REDIS_IMAGE = 'postgres:18.1-alpine3.23', 'redis:8.4-alpine'
INDEX_SQL = '249_add_usage_logs_api_key_cost_index_notx.sql'
INDEX_NAME = 'idx_usage_logs_api_key_created_cost'
DELTA = {INDEX_SQL, '250_openai_cache_write_ttl.sql', '251_group_force_openai_ultrafast.sql',
         '251_group_usage_rollup_nonblocking_build.sql', '252_custom_group_usage_rollup_rollback_floor_guard.sql'}
FUNCTIONS = {'invalidate_group_usage_rollup_state', 'invalidate_group_usage_rollup_state_after_insert',
             'invalidate_group_usage_rollups_at', 'group_usage_rollup_can_ignore_expired_changes'}
GO_SPACE = '\t\n\v\f\r \x85\xa0                　'
require, sha = evidence.require, evidence.sha


def sql_manifest(source):
    result = {}
    for path in sorted((source / 'backend/migrations').glob('*.sql')):
        raw = path.read_bytes()
        trimmed = raw.decode('utf-8').strip(GO_SPACE)
        if trimmed:
            result[path.name] = {'raw_sha256': sha(raw), 'checksum': sha(trimmed.encode())}
    return result


def check_migrations(old, new, ledger):
    require(set(new) - set(old) == DELTA and set(old) < set(new), 'unexpected migration delta')
    require(all(old[name] == new[name] for name in old), 'historical SQL changed')
    by_name = {row['filename']: row for row in ledger}
    require(len(by_name) == len(ledger) and set(by_name) == set(new), 'ledger filename set mismatch')
    for name, expected in new.items():
        require(by_name[name].get('checksum') == expected['checksum'] and
                bool(by_name[name].get('applied_at')), 'ledger checksum/applied_at mismatch')


def check_normal(sample, version):
    require(sample.get('health') == {'status': 'ok'}, 'health failed')
    require(sample.get('setup', {}).get('needs_setup') is False and
            sample['setup'].get('step') == 'completed', 'setup-only mode is not accepted')
    require(sample.get('public_version') == version, 'normal public version mismatch')
    require(sample.get('profile_status') == 401, 'normal protected route absent')
    require(bool(sample.get('entry_path')) and bool(sample.get('entry_sha256')), 'embedded frontend absent')


def background_complete(state, today, expected_cost, previous=None):
    if state.get('closed_before') != today or state.get('bucket_total') != expected_cost or \
            state.get('detail_total') != expected_cost or not state.get('computed_at'):
        return False
    return previous is None or (state['computed_at'] > previous['computed_at'] and
                                state['updated_at'] > previous['updated_at'])


def phase_counts(phases):
    """只统计 native 阶段标志，不伪装 Go test 的 run/pass 事件。"""
    executed = [value for value in phases.values() if value.get('executed') is True]
    verified = sum(value.get('verified') is True for value in executed)
    return {'executed': len(executed), 'verified': verified, 'unverified': len(executed) - verified,
            'not_executed': len(phases) - len(executed)}


def private_ipv4(value):
    address = ipaddress.IPv4Address(value)
    require(any(address in ipaddress.IPv4Network(cidr) for cidr in
                ('10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16')),
            'container address is not private IPv4')
    return address


def old_usage_contract(source):
    insert_path = source / 'backend/internal/repository/usage_log_repo_insert.go'
    select_path = source / 'backend/internal/repository/usage_log_repo_query.go'
    text = insert_path.read_text(encoding='utf-8')
    body = re.search(r'func \(r \*usageLogRepository\) createSingle\(.*?query := `([^`]+)`', text, re.S)
    projection = re.search(r'const usageLogSelectColumns = "([^"]+)"', select_path.read_text(encoding='utf-8'))
    require(body is not None and projection is not None, 'old SQL extraction failed')
    sql, columns = body[1], projection[1].split(', ')
    inserts = re.search(r'INSERT INTO usage_logs\s*\((.*?)\)\s*VALUES', sql, re.S)
    names = re.findall(r'\b[a-z][a-z_0-9]*\b', inserts[1])
    require(len(names) == 65 and len(columns) == 66 and columns == ['id'] + names,
            'd78 explicit column contract changed')
    require(set(map(int, re.findall(r'\$(\d+)', sql))) == set(range(1, 66)), 'old placeholders changed')
    return sql, columns, names, {'insert_source_sha256': sha(insert_path.read_bytes()),
                                'select_source_sha256': sha(select_path.read_bytes()),
                                'insert_sql_sha256': sha(sql.encode()), 'insert_args': 65, 'select_columns': 66}


class Probe:
    def __init__(self, root, artifacts):
        self.root, self.artifacts = root, artifacts
        self.owner = secrets.token_hex(6)
        self.prefix = 'sub2api-v0222-ci-' + self.owner
        self.containers, self.images, self.processes, self.volumes = [], [], [], []
        self.network = self.prefix + '-net'
        self.network_id = None
        self.work = Path(tempfile.mkdtemp(prefix='v0222-owned-', dir=root))
        self.data = self.work / 'data'
        self.stage = 'initialization'
        self.database = 'v0222_' + self.owner
        self.pg, self.redis = self.prefix + '-pg', self.prefix + '-redis'
        self.jwt_secret = secrets.token_hex(48)
        self.synthetic_email = 'ci-' + self.owner + '@example.invalid'
        self.synthetic_hash = secrets.token_hex(32)
        self.image_proofs, self.data_proofs, self.app_data_proofs = {}, {}, {}
        self.command_counts = {}
        self.receipt = {'verified': False, 'coverage': list('ABCDEF'),
            'stages': {name: {'executed': False, 'verified': False} for name in 'ABCDEF'},
            'native_probe_contract': 'v10-owned-ip-prebuild-redis-smoke',
            'old_commit': OLD_SHA, 'artifact_kind': 'rebuilt exact-source full release images',
            'production_original_binary_tested': False, 'commands': [],
            'process_E_executed': False, 'recovery_tested': False,
            'ledger_note': 'clean source-derived database; not production307/312',
            'no_egress_runtime_network': False, 'leader_locks_preserved': True,
            'network_smoke': {'executed': False, 'verified': False, 'deadline_seconds': 30}}
        self.index_receipt = {'verified': False, 'recovery_tested': False, 'native_execution': False,
            'steps': {name: {'executed': False, 'verified': False} for name in
                      ('old_normal_schema', 'original_runner_invalid', 'new_normal_recovery')}}

    def run(self, command, *, cwd=None, data=None, timeout=180, env=None):
        result = subprocess.run(command, cwd=cwd, input=data, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=timeout, env=env)
        self.command_counts[self.stage] = self.command_counts.get(self.stage, 0) + 1
        safe = []
        for argument in command:
            if argument.startswith('POSTGRES_PASSWORD='):
                argument = 'POSTGRES_PASSWORD=<ephemeral-redacted>'
            argument = argument.replace(str(self.work), '<owned-work>')
            safe.append(argument)
        # SQL stdin 仅保存 hash；不记录合成密码/hash/JWT。
        if data is None and (len(safe) < 3 or safe[1] != 'inspect'):
            self.receipt['commands'].append({'stage': self.stage, 'command': safe,
                'exit_code': result.returncode, 'stdout_sha256': sha(result.stdout),
                'stderr_sha256': sha(result.stderr)})
        if result.returncode:
            # argv 可能带合成密码，SQL 可能带合成 hash；只留安全分类和字节 hash。
            self.receipt['native_failure'] = {'stage': self.stage, 'executable': Path(command[0]).name,
                'exit_code': result.returncode, 'stdout_sha256': sha(result.stdout),
                'stderr_sha256': sha(result.stderr)}
            raise ValueError('subprocess failed at ' + self.stage)
        return result.stdout

    def docker(self, *args, timeout=180, env=None):
        return self.run(['docker', *args], timeout=timeout, env=env).decode().strip()

    def persist(self):
        self.receipt['stage_counts'] = phase_counts(self.receipt['stages'])
        self.receipt['count_semantics'] = 'A-F native phase flags, not Go test events'
        self.index_receipt['step_counts'] = phase_counts(self.index_receipt['steps'])
        self.index_receipt['case_counts'] = {'executed': int(self.index_receipt['steps']['old_normal_schema']['executed']),
            'verified': int(self.index_receipt['recovery_tested'] is True and all(
                value['verified'] is True for value in self.index_receipt['steps'].values()))}
        self.index_receipt['count_semantics'] = 'one native recovery scenario with derived step flags, not Go test events'
        evidence.save(self.artifacts / 'old-app-probe.json', self.receipt)
        evidence.save(self.artifacts / 'invalid-index-recovery.json', self.index_receipt)

    def complete(self, stage, **proof):
        self.receipt['stages'][stage] = {'executed': True, 'verified': True, **proof}
        self.persist()

    def archive(self, revision, destination):
        destination.mkdir()
        archive = self.work / (destination.name + '.tar')
        archive.write_bytes(self.run(['git', 'archive', '--format=tar', revision], cwd=self.root))
        with tarfile.open(archive) as source:
            source.extractall(destination, filter='data')
        return sha(archive.read_bytes())

    def image(self, source, revision, version, name):
        require(self.receipt['network_smoke']['verified'] is True,
                'refusing image build before owned host-network smoke')
        tag = self.prefix + ':' + name
        self.images.append(tag)
        builder = os.environ.get('BUILDX_BUILDER', '')
        require(builder, 'explicit Buildx builder missing')
        command = ['docker', 'buildx', 'build', '--builder', builder, '--load',
            '--file', str(source / 'Dockerfile'), '--platform', 'linux/amd64', '-t', tag,
            '--build-arg', 'VERSION=' + version, '--build-arg', 'COMMIT=' + revision,
            '--label', 'sub2api.ci.owner=' + self.owner,
            '--label', 'org.opencontainers.image.source=' + OCI_SOURCE,
            '--label', 'org.opencontainers.image.revision=' + revision,
            '--label', 'org.opencontainers.image.version=' + version, str(source)]
        self.run(command, timeout=2400)
        self.receipt['commands'].append({'stage': self.stage, 'command': command, 'exit_code': 0,
                                        'dockerfile_sha256': sha((source / 'Dockerfile').read_bytes())})
        return tag

    def image_identity(self, image, revision, version):
        inspected = json.loads(self.docker('image', 'inspect', image))[0]
        labels = inspected['Config']['Labels']
        require(inspected['Os'] == 'linux' and inspected['Architecture'] == 'amd64' and
                labels['sub2api.ci.owner'] == self.owner and
                labels['org.opencontainers.image.source'] == OCI_SOURCE and
                labels['org.opencontainers.image.revision'] == revision and
                labels['org.opencontainers.image.version'] == version, 'image identity mismatch')
        version_command = ['docker', 'run', '--rm', '--network', 'none', '--entrypoint',
                           '/app/sub2api', image, '-version']
        result = subprocess.run(version_command, capture_output=True, timeout=60)
        text = (result.stdout + result.stderr).decode()
        require(result.returncode == 0, 'binary version command failed')
        require(len(re.findall(r'Sub2API\s+' + re.escape(version) + r'\s+\(commit:\s*' +
                               revision + r',', text)) == 1, 'binary version/source mismatch')
        name = self.prefix + '-extract-' + revision[:8]
        self.containers.append(name)
        self.docker('create', '--name', name, '--label', 'sub2api.ci.owner=' + self.owner,
                    '--network', 'none', image)
        binary = self.work / (revision + '.bin')
        self.docker('cp', name + ':/app/sub2api', str(binary))
        proof = {'binary_sha256': sha(binary.read_bytes()), 'version': version, 'source_commit': revision,
                 'platform': 'linux/amd64', 'image_local_id': inspected['Id'], 'labels': {
                     key: labels[key] for key in ('org.opencontainers.image.source',
                     'org.opencontainers.image.revision', 'org.opencontainers.image.version')}}
        binary.unlink()
        self.docker('rm', name)
        self.image_proofs[image] = proof
        return proof

    def psql_argv(self, database=None):
        return ['docker', 'exec', '-i', self.pg, 'psql', '-X', '-qAt', '-v', 'ON_ERROR_STOP=1',
                '-U', 'postgres', '-d', database or self.database]

    def sql(self, statement, database=None):
        result = self.run(self.psql_argv(database), data=statement.encode()).decode().strip()
        self.receipt.setdefault('sql_inputs', {}).setdefault(self.stage, {})[sha(statement.encode())] = {
            'command': self.psql_argv(database), 'stdin_sha256': sha(statement.encode()), 'exit_code': 0}
        return result

    def query(self, statement, database=None):
        return json.loads(self.sql(statement, database))

    def identity(self, name):
        fields = self.docker('inspect', '--format',
            '{{.Id}}|{{.State.StartedAt}}|{{.RestartCount}}|{{.State.Status}}|{{.State.Pid}}', name).split('|')
        require(len(fields) == 5 and fields[3] == 'running' and fields[2] == '0' and
                fields[4].isdigit() and int(fields[4]) > 0, 'service stopped/restarted')
        return fields

    def assert_app_identity(self, name, image):
        inspected = json.loads(self.docker('inspect', name))[0]
        require(inspected['Config']['Labels'].get('sub2api.ci.owner') == self.owner and
                inspected['Image'] == self.image_proofs[image]['image_local_id'], 'app CID/image mismatch')
        binary_sha = self.docker('exec', name, 'sha256sum', '/app/sub2api').split()[0]
        require(binary_sha == self.image_proofs[image]['binary_sha256'], 'app binary identity mismatch')
        require(self.docker('exec', name, 'sha256sum', '/app/data/config.yaml').split()[0] ==
                self.app_data_proofs[name], 'app configuration changed')
        return {'container': self.identity(name), **self.image_proofs[image], 'binary_sha256': binary_sha,
                'configuration_sha256': self.app_data_proofs[name]}

    def snapshot(self, database=None):
        ledger = self.query("SELECT COALESCE(json_agg(x ORDER BY filename),'[]') FROM "
                            "(SELECT filename,checksum,applied_at FROM schema_migrations) x", database)
        atlas = self.query("SELECT COALESCE(json_agg(x ORDER BY version),'[]') FROM atlas_schema_revisions x", database)
        functions = self.query("SELECT json_object_agg(proname,md5(pg_get_functiondef(p.oid))) "
            "FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' "
            "AND proname IN ('invalidate_group_usage_rollup_state','invalidate_group_usage_rollup_state_after_insert',"
            "'invalidate_group_usage_rollups_at','group_usage_rollup_can_ignore_expired_changes')", database)
        return {'ledger': ledger, 'atlas': atlas, 'functions': functions}

    def invariant(self, snapshot, services):
        require(self.snapshot() == snapshot, 'ledger/Atlas/four functions changed')
        require(self.identity(self.pg) == services['pg'] and self.identity(self.redis) == services['redis'],
                'PG/Redis identity/start/restart changed')

    def wait(self, condition, timeout=180):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            result = condition()
            if result:
                return result
            time.sleep(0.25)
        raise ValueError('condition deadline at ' + self.stage)

    def endpoint(self, name, timeout=15):
        require(name in self.containers and re.fullmatch(r'[0-9a-f]{64}', self.network_id or ''),
                'unregistered container or network')
        require(timeout > 0, 'endpoint deadline exceeded')
        started = time.monotonic()
        containers = json.loads(self.docker('inspect', name, timeout=timeout))
        remaining = timeout - (time.monotonic() - started)
        require(remaining > 0, 'endpoint deadline exceeded')
        networks = json.loads(self.docker('network', 'inspect', self.network_id, timeout=remaining))
        require(len(containers) == len(networks) == 1, 'ambiguous container/network inspect')
        container, network = containers[0], networks[0]
        require(network.get('Id') == self.network_id and network.get('Name') == self.network and
                network.get('Labels', {}).get('sub2api.ci.owner') == self.owner and
                network.get('Internal') is True and network.get('Driver') == 'bridge' and
                network.get('Scope') == 'local', 'owned internal bridge identity mismatch')
        require(container.get('Name') == '/' + name and
                container.get('Config', {}).get('Labels', {}).get('sub2api.ci.owner') == self.owner and
                container.get('State', {}).get('Running') is True and container.get('RestartCount') == 0,
                'owned live container identity mismatch')
        cid = container.get('Id')
        require(isinstance(cid, str) and re.fullmatch(r'[0-9a-f]{64}', cid), 'invalid owned container CID')
        host = container.get('HostConfig', {})
        require(host.get('NetworkMode') in (self.network, self.network_id) and
                not host.get('PortBindings') and not host.get('PublishAllPorts'), 'unexpected network/port publication')
        settings = container.get('NetworkSettings', {})
        attached = settings.get('Networks', {})
        require(set(attached) == {self.network} and not any((settings.get('Ports') or {}).values()),
                'container must attach only the owned network without published ports')
        endpoint = attached[self.network]
        require(endpoint.get('NetworkID') == self.network_id and
                re.fullmatch(r'[0-9a-f]{64}', endpoint.get('EndpointID', '')),
                'container endpoint belongs to another network')
        address = private_ipv4(endpoint.get('IPAddress'))
        member = network.get('Containers', {}).get(cid, {})
        require(member.get('Name') == name and member.get('EndpointID') == endpoint['EndpointID'],
                'owned network has no matching container endpoint')
        interface = ipaddress.IPv4Interface(member.get('IPv4Address'))
        require(interface.ip == address and interface.network.prefixlen == endpoint.get('IPPrefixLen') and
                address not in (interface.network.network_address, interface.network.broadcast_address),
                'container IP/network membership mismatch')
        require(any(ipaddress.IPv4Network(item.get('Subnet', ''), strict=False) == interface.network
                    for item in network.get('IPAM', {}).get('Config', []) if ':' not in item.get('Subnet', '')),
                'container IP is outside the owned subnet')
        require(str(address) != endpoint.get('Gateway'), 'container IP must not be bridge gateway')
        return {'container_name': name, 'container_id': cid, 'network_id': self.network_id,
                'endpoint_id': endpoint['EndpointID'], 'ipv4': str(address)}

    def http(self, app, path, *, token=None, body=None, method=None):
        address = self.endpoint(app)['ipv4']
        headers = {'User-Agent': 'v0222-isolated-ci'}
        if token:
            headers['Authorization'] = 'Bearer ' + token
        if body is not None:
            headers['Content-Type'] = 'application/json'
        request = urllib.request.Request('http://' + address + ':8080' + path, headers=headers,
            data=json.dumps(body).encode() if body is not None else None, method=method)
        try:
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            with opener.open(request, timeout=5) as response:
                return response.status, response.read(16 * 1024 * 1024), response.headers.get('Content-Type', '')
        except urllib.error.HTTPError as error:
            return error.code, b'', ''

    def normal(self, app, version):
        def sample():
            self.identity(app)
            try:
                status, health, _ = self.http(app, '/health')
                if status != 200:
                    return None
                setup_status, setup, _ = self.http(app, '/setup/status')
                public_status, public, _ = self.http(app, '/api/v1/settings/public')
                profile_status, _, _ = self.http(app, '/api/v1/user/profile')
                index_status, index, _ = self.http(app, '/')
                entry = re.search(rb'(?:src|href)="(/assets/index-[A-Za-z0-9_-]+\.js)"', index)
                if not entry or index_status != 200 or setup_status != 200 or public_status != 200:
                    return None
                entry_path = entry[1].decode('ascii')
                entry_status, entry_bytes, entry_type = self.http(app, entry_path)
                require(entry_status == 200 and entry_bytes and entry_type.split(';')[0].strip() in
                        ('application/javascript', 'text/javascript'), 'embedded asset unavailable')
                value = {'health': json.loads(health), 'setup': json.loads(setup).get('data', {}),
                    'public_version': json.loads(public).get('data', {}).get('version'),
                    'profile_status': profile_status, 'entry_path': entry_path, 'entry_sha256': sha(entry_bytes)}
                check_normal(value, version)
                return value
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                return None
        return self.wait(sample)

    def token(self):
        # 精确匹配旧/新 resolvedTokenVersion；仅合成 secret，token 从不写制品/argv/log。
        version = int.from_bytes(hashlib.sha256((self.synthetic_email.lower() + '\n' +
                                                self.synthetic_hash).encode()).digest()[:8], 'big') & 0x7fffffffffffffff
        now = int(time.time())
        claims = {'user_id': 900001, 'email': self.synthetic_email, 'role': 'admin',
                  'token_version': version, 'iat': now, 'nbf': now - 1, 'exp': now + 120}
        encode = lambda data: base64.urlsafe_b64encode(json.dumps(data, separators=(',', ':')).encode()).rstrip(b'=')
        unsigned = encode({'alg': 'HS256', 'typ': 'JWT'}) + b'.' + encode(claims)
        return (unsigned + b'.' + base64.urlsafe_b64encode(hmac.new(
            self.jwt_secret.encode(), unsigned, hashlib.sha256).digest()).rstrip(b'=')).decode()

    def business(self, app, expected, group=900001):
        token = self.token()
        status, raw, _ = self.http(app, '/api/v1/user/profile', token=token)
        require(status == 200 and json.loads(raw).get('data', {}).get('id') == 900001,
                'authenticated synthetic profile failed')
        marker = 'ci-profile-' + secrets.token_hex(4)
        status, _, _ = self.http(app, '/api/v1/user', token=token, body={'username': marker}, method='PUT')
        require(status == 200 and self.sql('SELECT username FROM users WHERE id=900001') == marker,
                'authenticated write did not persist')
        status, raw, _ = self.http(app, '/api/v1/admin/compliance', token=token)
        require(status == 200, 'synthetic compliance status failed')
        compliance = json.loads(raw)['data']
        if compliance['required']:
            status, _, _ = self.http(app, '/api/v1/admin/compliance/accept', token=token,
                body={'phrase': compliance['ack_phrase_en'], 'language': 'en'}, method='POST')
            require(status == 200, 'synthetic compliance accept failed')
        status, raw, _ = self.http(app, '/api/v1/admin/groups/usage-summary', token=token)
        values = json.loads(raw)['data'] if status == 200 else []
        selected = [value for value in values if value.get('group_id') == group]
        require(status == 200 and len(selected) == 1 and selected[0]['total_cost'] == expected,
                'authenticated old/new summary inconsistent')
        return {'profile_status': 200, 'profile_write_status': 200, 'persisted_write': True,
                'group_summary_status': 200, 'group_id': group, 'total_cost': expected,
                'token_ttl_seconds': 120, 'raw_response_recorded': False}

    def app(self, image, name, data=None):
        self.containers.append(name)
        data = data or self.data
        self.app_data_proofs[name] = self.data_proofs[str(data)]
        self.docker('run', '-d', '--name', name, '--label', 'sub2api.ci.owner=' + self.owner,
            '--network', self.network, '-v', str(data) + ':/app/data',
            '-e', 'DATA_DIR=/app/data', '-e', 'CONFIG_FILE=/app/data/config.yaml',
            '-e', 'RUN_MODE=standard', '-e', 'CONCURRENCY_SKIP_STARTUP_CLEANUP=true', image)
        return name

    def terminate(self, app, crash=False):
        inspected = json.loads(self.docker('inspect', app))[0]
        require(inspected['Config']['Labels'].get('sub2api.ci.owner') == self.owner,
                'refuse to signal an unowned container')
        command = ['docker', 'kill', '--signal=' + ('KILL' if crash else 'TERM'), inspected['Id']]
        self.run(command)
        self.wait(lambda: self.docker('inspect', '--format', '{{.State.Running}}', app) == 'false', timeout=60)
        code = int(self.docker('inspect', '--format', '{{.State.ExitCode}}', app))
        require(code == (137 if crash else 0), 'unexpected app termination result')
        log_result = subprocess.run(['docker', 'logs', app], capture_output=True, timeout=30)
        logs = log_result.stdout + log_result.stderr
        require(log_result.returncode == 0, 'app termination logs unavailable')
        critical = len(re.findall(rb'(?im)(?:^|\n)panic:|fatal error:|WARNING: DATA RACE', logs))
        require(critical == 0, 'panic/fatal/race observed in app')
        if not crash:
            require(b'[Cleanup] All cleanup steps completed' in logs and
                    b'[Cleanup] Warning: cleanup timed out' not in logs and
                    re.search(rb'\[Cleanup\][^\r\n]* failed:', logs) is None and
                    b'Server exited' in logs, 'normal app cleanup not complete')
        return {'command': command, 'command_exit_code': 0, 'app_exit_code': code,
                'log_sha256': sha(logs), 'critical_count': critical,
                'full_cleanup_observed': not crash,
                'classification': 'controlled_candidate_crash_after_real_build_error' if crash else 'graceful_SIGTERM'}

    def state(self, group=900001):
        require(group in (900001, 900002), 'unexpected synthetic group')
        return self.query("SELECT json_build_object('closed_before',closed_before::text,"
            "'building_until',building_until::text,'build_dirty_from',build_dirty_from::text,"
            "'updated_at',updated_at::text,'computed_at',(SELECT max(computed_at)::text FROM usage_group_daily_rollups WHERE group_id="
            + str(group) + "),'bucket_total',(SELECT COALESCE(sum(actual_cost),0)::float8 FROM usage_group_daily_rollups WHERE group_id="
            + str(group) + "),'detail_total',(SELECT COALESCE(sum(actual_cost),0)::float8 FROM usage_logs WHERE group_id="
            + str(group) + ")) FROM usage_group_rollup_state WHERE id=1")

    def cycle(self, app, today, cost, previous=None, group=900001, timeout=240):
        identity = self.identity(app)
        result = self.wait(lambda: state if background_complete(state := self.state(group), today, cost, previous) else None,
                           timeout=timeout)
        require(self.identity(app) == identity, 'background app restarted')
        # 总额相等之外，逐日 FULL JOIN 防止成本在日桶间错位。
        mismatch = self.sql("WITH detail AS (SELECT (created_at AT TIME ZONE 'UTC')::date bucket_date,"
            "sum(actual_cost) actual_cost FROM usage_logs WHERE group_id=" + str(group) +
            " AND created_at < date_trunc('day',now()) GROUP BY 1), buckets AS (SELECT bucket_date,actual_cost "
            "FROM usage_group_daily_rollups WHERE group_id=" + str(group) + ") SELECT count(*) FROM detail FULL JOIN buckets "
            "USING(bucket_date) WHERE detail.actual_cost IS DISTINCT FROM buckets.actual_cost")
        require(mismatch == '0', 'daily bucket mismatch')
        return {**result, 'app_identity': identity, 'daily_mismatch_count': 0,
                'business': self.business(app, cost, group)}

    def config(self, directory, database, catalog):
        directory.mkdir(mode=0o700)
        pricing = directory / 'pricing'
        pricing.mkdir()
        shutil.copyfile(catalog, pricing / 'model_pricing.json')
        value = {'run_mode': 'standard', 'timezone': 'UTC',
            'server': {'host': '0.0.0.0', 'port': 8080, 'mode': 'release'},
            'jwt': {'secret': self.jwt_secret, 'access_token_expire_minutes': 2},
            'database': {'host': 'database', 'port': 5432, 'user': 'postgres', 'password': self.password,
                'dbname': database, 'sslmode': 'disable', 'max_open_conns': 8, 'max_idle_conns': 2},
            'redis': {'host': 'redis', 'port': 6379, 'db': 0, 'pool_size': 8, 'min_idle_conns': 1},
            'pricing': {'remote_url': '', 'hash_url': '', 'data_dir': '/app/data/pricing',
                'fallback_file': '/app/data/pricing/model_pricing.json', 'update_interval_hours': 8760},
            'dashboard_aggregation': {'enabled': True, 'interval_seconds': 2, 'lookback_seconds': 3600}}
        (directory / 'config.yaml').write_text(json.dumps(value) + '\n', encoding='utf-8')
        (directory / 'config.yaml').chmod(0o600)
        self.data_proofs[str(directory)] = sha((directory / 'config.yaml').read_bytes())
        (directory / '.installed').write_text('isolated-ci-installed-fixture\n', encoding='utf-8')
        return {'timezone': 'UTC', 'standard_mode': True, 'dashboard_interval_seconds': 2,
                'pricing_remote_url_empty': True, 'pricing_hash_url_empty': True,
                'config_content_recorded': False}

    def old_sql_roundtrip(self, old):
        sql, projection, names, proof = old_usage_contract(old)
        values = {name: 'NULL' for name in names}
        values.update({name: '0' for name in ('input_tokens','output_tokens','cache_creation_tokens','cache_read_tokens',
            'cache_creation_5m_tokens','cache_creation_1h_tokens','image_output_tokens','image_output_cost',
            'image_input_tokens','image_input_cost','input_cost','output_cost','cache_creation_cost','cache_read_cost',
            'total_cost','billing_type','request_type','image_count','video_count')})
        values.update({name: 'FALSE' for name in ('stream','openai_ws_mode','cache_ttl_overridden',
                                                'long_context_billing_applied','native_compaction_v2')})
        values.update(user_id='900001', api_key_id='900001', account_id='900001', group_id='900001',
            request_id="'ci-oldboot-fixture'", model="'ci-model'", requested_model="'ci-model'", actual_cost='1.25',
            rate_multiplier='1', account_rate_multiplier='1',
            created_at="date_trunc('day',now())-interval '2 days'+interval '1 hour'")
        rendered = re.sub(r'\$(\d+)\b', lambda match: values[names[int(match[1]) - 1]], sql)
        first = self.sql(rendered)
        require(first and self.sql(rendered) == '', 'old ON CONFLICT duplicate contract failed')
        row = self.query('SELECT row_to_json(x) FROM (SELECT ' + ', '.join(projection) +
                         " FROM usage_logs WHERE request_id='ci-oldboot-fixture') x")
        require(len(row) == 66 and row['actual_cost'] == 1.25 and row['requested_model'] == 'ci-model',
                'old SELECT projection failed')
        ttl = self.query("SELECT json_build_array(cache_creation_30m_tokens,cache_creation_12h_tokens) "
                         "FROM usage_logs WHERE request_id='ci-oldboot-fixture'")
        require(ttl == [0, 0], 'new TTL defaults do not preserve old INSERT')
        self.sql("UPDATE usage_logs SET cache_creation_30m_tokens=23,cache_creation_12h_tokens=29 "
                 "WHERE request_id='ci-oldboot-fixture'")
        selected = self.query('SELECT row_to_json(x) FROM (SELECT ' + ', '.join(projection) +
                              " FROM usage_logs WHERE request_id='ci-oldboot-fixture') x")
        require(selected == row, 'new TTL changed old projection')
        ttl_values = self.query("SELECT json_build_array(cache_creation_30m_tokens,cache_creation_12h_tokens) "
                                "FROM usage_logs WHERE request_id='ci-oldboot-fixture'")
        require(ttl_values == [23, 29], 'explicit new TTL values did not persist')
        return {**proof, 'old_insert_select_executed': True, 'deduplicated_row_count': 1,
                'new_ttl_defaults': ttl, 'new_ttl_values': ttl_values,
                'level': 'source-extracted SQL against real migrated app DB; not app usage writer'}

    def leaders(self):
        # 只查租约 TTL，不读取/删除 owner 值，不清 Redis。
        keys = ('leader:lock:dashboard:aggregation:leader',
                'leader:lock:dashboard:aggregation:group-usage-backfill:leader')
        return {key: int(self.docker('exec', self.redis, 'redis-cli', 'PTTL', key)) for key in keys}

    def failed_candidate(self, new_image, old_image, version, snapshot, services, today):
        self.stage = 'E-real-new-background-failure'
        self.receipt['stages']['E']['executed'] = True
        self.sql("INSERT INTO groups(id,name,platform) VALUES(900002,'ci-failed-candidate','anthropic');"
            "INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,group_id,actual_cost,created_at) "
            "VALUES(900001,900001,900001,'ci-failed-candidate','ci-model',900002,4,"
            "date_trunc('day',now())-interval '2 days'+interval '2 hours');"
            "CREATE SEQUENCE ci_failed_build_attempts;"
            "CREATE FUNCTION ci_fail_candidate_build() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN "
            "IF NEW.group_id=900002 THEN PERFORM nextval('ci_failed_build_attempts');"
            "RAISE EXCEPTION 'isolated CI candidate compute failure' USING ERRCODE='40001'; END IF; RETURN NEW; END $$;"
            "CREATE TRIGGER ci_fail_candidate_build BEFORE INSERT ON usage_group_daily_rollups "
            "FOR EACH ROW EXECUTE FUNCTION ci_fail_candidate_build();")
        candidate = self.app(new_image, self.prefix + '-failed-candidate')
        normal = self.normal(candidate, version)
        identity = self.assert_app_identity(candidate, new_image)
        failed = self.wait(lambda: state if (state := self.state(900002)).get('building_until') and
            self.sql('SELECT is_called FROM ci_failed_build_attempts') == 't' else None)
        # 先确认真实后台 SQL 已结束且两种 lease 由 app 自己释放，再异常退出；不删 lease。
        def leases_released():
            values = self.leaders()
            return values if all(ttl == -2 for ttl in values.values()) else None
        released = self.wait(leases_released)
        # 真 SQL 失败不终止产品服务；这里只注入 owned crash，不能称自然 fatal。
        crash = self.terminate(candidate, crash=True)
        lease_after_exit = self.leaders()
        require(self.state(900002)['building_until'] == failed['building_until'], 'failed registration lost')
        self.sql("UPDATE usage_logs SET actual_cost=4.5 WHERE request_id='ci-failed-candidate';")
        persisted = self.state(900002)
        require(persisted['building_until'] and persisted['build_dirty_from'], 'failed/dirty state not retained')
        self.invariant(snapshot, services)
        self.sql('DROP TRIGGER ci_fail_candidate_build ON usage_group_daily_rollups; DROP FUNCTION ci_fail_candidate_build();')
        self.stage = 'E-old-restart-on-failed-registration'
        old_app = self.app(old_image, self.prefix + '-old-after-failure')
        old_normal = self.normal(old_app, OLD_VERSION)
        old_identity = self.assert_app_identity(old_app, old_image)
        # 若 crash 恰逢周期重新获取 lease，只等它真实过期；绝不删 lease 或改 TTL。
        first = self.cycle(old_app, today, 4.5, group=900002, timeout=360)
        require(self.state(900002)['building_until'] == persisted['building_until'],
                'old app unexpectedly cleared new registration')
        self.sql("UPDATE usage_logs SET actual_cost=6 WHERE request_id='ci-failed-candidate'")
        second = self.cycle(old_app, today, 6, first, 900002)
        self.invariant(snapshot, services)
        old_exit = self.terminate(old_app)
        self.stage = 'E-new-normal-recovery'
        recovery = self.app(new_image, self.prefix + '-new-recovery')
        recovered_normal = self.normal(recovery, version)
        recovered_identity = self.assert_app_identity(recovery, new_image)
        recovered = self.wait(lambda: state if background_complete(state := self.state(900002), today, 6) and
                              state['building_until'] is None and state['build_dirty_from'] is None else None)
        business = self.business(recovery, 6, 900002)
        # 不是仅清登记：恢复的新完整 app 再执行两次真正构建，仍按 old→new 原顺序。
        self.sql("UPDATE usage_logs SET actual_cost=7 WHERE request_id='ci-failed-candidate'")
        new_first = self.cycle(recovery, today, 7, recovered, 900002)
        self.sql("UPDATE usage_logs SET actual_cost=8 WHERE request_id='ci-failed-candidate'")
        new_second = self.cycle(recovery, today, 8, new_first, 900002)
        require(self.state(900002)['building_until'] is None and self.state(900002)['build_dirty_from'] is None,
                'new recovery left build metadata')
        self.invariant(snapshot, services)
        recovery_exit = self.terminate(recovery)
        self.complete('E', failed_candidate={'normal': normal, 'identity': identity, 'failed_build_state': failed,
            'exit': crash, 'persisted_dirty_state': persisted, 'leases_released_before_exit': released,
            'lease_ttl_ms_after_exit': lease_after_exit}, old_restart={'normal': old_normal,
            'identity': old_identity, 'cycle_1': first, 'cycle_2': second, 'exit': old_exit},
            new_recovery={'normal': recovered_normal, 'identity': recovered_identity, 'state': recovered,
                          'business': business, 'cycle_1': new_first, 'cycle_2': new_second, 'exit': recovery_exit})
        self.receipt['process_E_executed'] = True

    def invalid_index(self, old, new, old_image, new_image, version, services):
        self.stage = 'invalid-index-old-normal-schema'
        self.index_receipt['steps']['old_normal_schema']['executed'] = True
        database = 'idx_' + self.owner
        self.sql('CREATE DATABASE ' + database, 'postgres')
        data = self.work / 'index-data'
        self.config(data, database, old / 'backend/resources/model-pricing/model_prices_and_context_window.json')
        old_app = self.app(old_image, self.prefix + '-index-old', data)
        self.normal(old_app, OLD_VERSION)
        self.assert_app_identity(old_app, old_image)
        old_snapshot = self.snapshot(database)
        require({row['filename']: row['checksum'] for row in old_snapshot['ledger']} ==
                {name: row['checksum'] for name, row in sql_manifest(old).items()}, 'index old baseline mismatch')
        self.terminate(old_app)
        self.index_receipt['steps']['old_normal_schema']['verified'] = True
        raw = (new / 'backend/migrations' / INDEX_SQL).read_bytes()
        official = self.run(['git', 'show', OFFICIAL_SHA + ':backend/migrations/' + INDEX_SQL], cwd=self.root)
        require(raw == official, 'official 249 SQL bytes changed')
        self.index_receipt.update({key: self.receipt[key] for key in ('source_commit','github_sha','runner_version','inputs','run_url')})
        self.index_receipt.update(native_execution=True, command=['full new app original ApplyMigrations runs exact official 249',
            'pg_cancel_backend after waiting-for-writers phase; require candidate failure exit',
            'full new normal app ApplyMigrations retry'],
            sql_raw_sha256=sha(raw), sql_checksum=sha(raw.decode().strip(GO_SPACE).encode()), commands=[])
        self.persist()
        self.stage = 'invalid-index-real-concurrent-create-cancel'
        self.index_receipt['steps']['original_runner_invalid']['executed'] = True
        blocker = subprocess.Popen(self.psql_argv(database), stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.processes.append(blocker)
        blocker.stdin.write(b"BEGIN; SET application_name='v0222_index_blocker'; LOCK TABLE usage_logs IN ROW EXCLUSIVE MODE;\n")
        blocker.stdin.flush()
        self.wait(lambda: self.sql("SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a USING(pid) "
            "WHERE a.application_name='v0222_index_blocker' AND l.mode='RowExclusiveLock' AND l.granted "
            "AND l.relation='usage_logs'::regclass", database) == '1')
        failing_app = self.app(new_image, self.prefix + '-index-failed-candidate', data)
        candidate_endpoint = self.endpoint(failing_app)
        failing_inspect = json.loads(self.docker('inspect', candidate_endpoint['container_id']))[0]
        app_ip = candidate_endpoint['ipv4']
        require(failing_inspect['Image'] == self.image_proofs[new_image]['image_local_id'],
                'index candidate process/image/network mismatch')
        def pending():
            rows = self.query("SELECT COALESCE(json_agg(x),'[]') FROM (SELECT p.pid,p.phase,i.indexrelid::oid::text oid,"
                "i.indisvalid,md5(a.query) query_md5 FROM pg_stat_progress_create_index p "
                "JOIN pg_stat_activity a USING(pid) JOIN pg_index i ON i.indexrelid=p.index_relid "
                "WHERE a.client_addr='" + app_ip + "'::inet AND a.datname='" + database + "' "
                "AND p.phase='waiting for writers before build' AND i.indexrelid='" + INDEX_NAME + "'::regclass "
                "AND a.query LIKE '%CREATE INDEX CONCURRENTLY IF NOT EXISTS " + INDEX_NAME + "%') x", database)
            return rows[0] if len(rows) == 1 and not rows[0]['indisvalid'] else None
        # to_regclass 防止首次 catalog 尚无 index 时将轮询误报为 SQL 错误。
        self.wait(lambda: self.sql("SELECT to_regclass('public." + INDEX_NAME + "') IS NOT NULL", database) == 't')
        pending_state = self.wait(pending)
        require(self.sql('SELECT pg_cancel_backend(' + str(int(pending_state['pid'])) + ')', database) == 't', 'cancel failed')
        self.wait(lambda: self.docker('inspect', '--format', '{{.State.Running}}', failing_app) == 'false', timeout=60)
        exit_code = int(self.docker('inspect', '--format', '{{.State.ExitCode}}', failing_app))
        require(exit_code != 0, 'canceled migration candidate did not fail initialization')
        log_result = subprocess.run(['docker', 'logs', failing_app], capture_output=True, timeout=30)
        error = log_result.stdout + log_result.stderr
        require(log_result.returncode == 0, 'candidate failure log unavailable')
        require(INDEX_SQL.encode() in error and b'canceling statement due to user request' in error,
                'candidate exit did not identify official 249 cancellation')
        blocker.stdin.write(b'ROLLBACK;\n')
        blocker.stdin.close()
        require(blocker.wait(timeout=30) == 0, 'writer blocker did not release')
        invalid = self.query("SELECT json_build_object('oid',i.indexrelid::oid::text,'valid',i.indisvalid,"
            "'ledger_count',(SELECT count(*) FROM schema_migrations WHERE filename='" + INDEX_SQL + "')) "
            "FROM pg_index i WHERE i.indexrelid='" + INDEX_NAME + "'::regclass", database)
        require(invalid['valid'] is False and invalid['ledger_count'] == 0 and invalid['oid'] == pending_state['oid'],
                'invalid catalog state/absent ledger not observed')
        self.index_receipt.update(invalid_before_retry=invalid, cancellation_exit_code=exit_code,
            canceled_candidate={'container_id': failing_inspect['Id'], 'image_local_id': failing_inspect['Image'],
                               'source_commit': self.image_proofs[new_image]['source_commit'],
                               'binary_sha256': self.image_proofs[new_image]['binary_sha256']},
            original_runner_query_md5=pending_state['query_md5'], cancellation_stderr_sha256=sha(error),
            phase=pending_state['phase'])
        self.index_receipt['steps']['original_runner_invalid']['verified'] = True
        self.persist()
        self.stage = 'invalid-index-original-runner-recovery'
        self.index_receipt['steps']['new_normal_recovery']['executed'] = True
        new_app = self.app(new_image, self.prefix + '-index-new', data)
        normal = self.normal(new_app, version)
        identity = self.assert_app_identity(new_app, new_image)
        after = self.snapshot(database)
        check_migrations(sql_manifest(old), sql_manifest(new), after['ledger'])
        require([row for row in after['ledger'] if row['filename'] in sql_manifest(old)] == old_snapshot['ledger'],
                'old ledger changed on invalid retry')
        require(after['atlas'] == old_snapshot['atlas'], 'Atlas changed on retry')
        valid = self.query("SELECT json_build_object('oid',i.indexrelid::oid::text,'valid',i.indisvalid,"
            "'definition',pg_get_indexdef(i.indexrelid)) FROM pg_index i WHERE i.indexrelid='" + INDEX_NAME + "'::regclass", database)
        require(valid['valid'] is True and valid['oid'] != invalid['oid'] and
                'INCLUDE (actual_cost)' in valid['definition'], 'original runner did not replace invalid index')
        self.terminate(new_app)
        require(self.identity(self.pg) == services['pg'] and self.identity(self.redis) == services['redis'], 'service changed')
        self.index_receipt['steps']['new_normal_recovery']['verified'] = True
        self.index_receipt.update(verified=True, recovery_tested=True, exit_code=0, valid_after_retry=valid,
            new_normal=normal, identity=identity, old_ledger_preserved=True, atlas_preserved=True,
            ledger=after['ledger'])
        self.receipt['recovery_tested'] = True
        self.persist()

    def host_network_smoke(self):
        self.stage = 'owned-internal-host-tcp-smoke'
        started = time.monotonic()
        deadline = started + 30
        smoke = self.receipt['network_smoke']
        smoke.update(executed=True, verified=False, protocol='redis-resp-PING/PONG',
                     host_direct_container_ip=True, published_ports=False, attempts=0)
        self.persist()
        last_failure = 'deadline'
        try:
            while time.monotonic() < deadline:
                smoke['attempts'] += 1
                endpoint = self.endpoint(self.redis, timeout=min(5, deadline - time.monotonic()))
                try:
                    remaining = deadline - time.monotonic()
                    require(remaining > 0, 'smoke deadline exceeded')
                    with socket.create_connection((endpoint['ipv4'], 6379), timeout=min(1, remaining)) as connection:
                        connection.settimeout(min(1, deadline - time.monotonic()))
                        connection.sendall(b'*1\r\n$4\r\nPING\r\n')
                        received = b''
                        while b'\r\n' not in received and len(received) < 64:
                            remaining = deadline - time.monotonic()
                            require(remaining > 0, 'smoke deadline exceeded')
                            connection.settimeout(min(1, remaining))
                            part = connection.recv(64 - len(received))
                            if not part:
                                break
                            received += part
                    require(received == b'+PONG\r\n', 'smoke did not receive Redis PONG')
                    require(time.monotonic() <= deadline, 'smoke exceeded 30 seconds')
                    smoke.update(verified=True, exit_code=0, endpoint=endpoint,
                                 response_sha256=sha(received), elapsed_seconds=time.monotonic() - started)
                    self.persist()
                    return
                except OSError as error:
                    last_failure = type(error).__name__
                remaining = deadline - time.monotonic()
                if remaining > 0:
                    time.sleep(min(0.25, remaining))
            raise ValueError('host to owned internal Redis PING deadline exceeded')
        except Exception as error:
            smoke.update(verified=False, exit_code=1, failure_class=type(error).__name__,
                         last_connection_failure=last_failure, elapsed_seconds=time.monotonic() - started)
            self.persist()
            raise

    def prepare_runtime_network(self):
        self.stage = 'owned-internal-redis-setup'
        self.docker('pull', REDIS_IMAGE, timeout=300)
        self.network_id = self.docker('network', 'create', '--driver', 'bridge', '--internal',
            '--label', 'sub2api.ci.owner=' + self.owner, self.network)
        require(re.fullmatch(r'[0-9a-f]{64}', self.network_id), 'invalid created network ID')
        redis_volume = self.prefix + '-redis-data'
        self.volumes.append(redis_volume)
        self.docker('volume', 'create', '--label', 'sub2api.ci.owner=' + self.owner, redis_volume)
        self.containers.append(self.redis)
        self.docker('run', '-d', '--name', self.redis, '--label', 'sub2api.ci.owner=' + self.owner,
            '--network', self.network, '--network-alias', 'redis', '-v', redis_volume + ':/data', REDIS_IMAGE)
        self.host_network_smoke()
        # endpoint() 已核 owner/internal/bridge/唯一网络；smoke 先真通过才允许构建。
        self.receipt['no_egress_runtime_network'] = True
        self.receipt['redis_before_build'] = self.identity(self.redis)
        self.persist()

    def start_postgres(self):
        # -e NAME 由 Docker CLI 子进程环境取值；密码不进入 argv 或命令收据。
        pg_volume = self.prefix + '-pg-data'
        self.volumes.append(pg_volume)
        self.docker('volume', 'create', '--label', 'sub2api.ci.owner=' + self.owner, pg_volume)
        self.containers.append(self.pg)
        self.docker('run', '-d', '--name', self.pg, '--label', 'sub2api.ci.owner=' + self.owner,
            '--network', self.network, '--network-alias', 'database', '-v', pg_volume + ':/var/lib/postgresql',
            '-e', 'POSTGRES_DB=' + self.database, '-e', 'POSTGRES_PASSWORD', PG_IMAGE,
            env=dict(os.environ, POSTGRES_PASSWORD=self.password))

    def build_release_images(self, old, new, final, version):
        self.prepare_runtime_network()
        require(self.receipt['network_smoke']['verified'] is True, 'network smoke must precede image builds')
        self.stage = 'full-release-image-builds'
        old_image = self.image(old, OLD_SHA, OLD_VERSION, 'old')
        new_image = self.image(new, final, version, 'new')
        return old_image, new_image

    def execute(self):
        paths = [*Path(__file__).parent.glob('v0222_*'), self.root / '.github/workflows/backend-ci.yml',
                 *self.root.glob('backend/migrations/*.sql'), self.root / 'Dockerfile',
                 self.root / 'backend/internal/repository/migrations_runner.go']
        self.receipt.update(evidence.context(self.root, paths))
        final = self.receipt['source_commit']
        self.stage = 'exact-source-archive'
        self.run(['git', 'merge-base', '--is-ancestor', OLD_SHA, final], cwd=self.root)
        self.run(['git', 'merge-base', '--is-ancestor', OFFICIAL_SHA, final], cwd=self.root)
        old, new = self.work / 'old', self.work / 'new'
        self.receipt['old_archive_sha256'] = self.archive(OLD_SHA, old)
        self.receipt['new_archive_sha256'] = self.archive(final, new)
        old_sql, new_sql = sql_manifest(old), sql_manifest(new)
        require(set(new_sql) - set(old_sql) == DELTA and all(old_sql[k] == new_sql[k] for k in old_sql), 'SQL delta mismatch')
        require((old / 'backend/cmd/server/VERSION').read_text().strip() == OLD_VERSION, 'old embedded version mismatch')
        self.receipt.update(migration_manifest=new_sql, old_embedded_sql_count=len(old_sql), new_embedded_sql_count=len(new_sql))
        version = (new / 'backend/cmd/server/VERSION').read_text().strip()
        old_image, new_image = self.build_release_images(old, new, final, version)
        self.receipt['old_binary'] = self.image_identity(old_image, OLD_SHA, OLD_VERSION)
        self.receipt['new_binary'] = self.image_identity(new_image, final, version)
        self.docker('pull', PG_IMAGE, timeout=300)
        self.stage = 'isolated-services'
        require(self.identity(self.redis) == self.receipt['redis_before_build'],
                'smoke Redis changed during image builds')
        self.endpoint(self.redis)
        self.password = secrets.token_hex(24)
        self.start_postgres()
        self.wait(lambda: subprocess.run(['docker','exec',self.pg,'pg_isready','-U','postgres','-d',self.database],
                                       stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL).returncode == 0)
        self.wait(lambda: self.docker('exec',self.redis,'redis-cli','ping') == 'PONG')
        services = {'pg': self.identity(self.pg), 'redis': self.identity(self.redis)}
        self.receipt['configuration'] = self.config(self.data, self.database,
            old / 'backend/resources/model-pricing/model_prices_and_context_window.json')
        self.stage = 'A-new-normal-bootstrap'
        self.receipt['stages']['A']['executed'] = True
        new_app = self.app(new_image, self.prefix + '-new')
        new_normal = self.normal(new_app, version)
        new_identity = self.assert_app_identity(new_app, new_image)
        snapshot = self.snapshot()
        check_migrations(old_sql, new_sql, snapshot['ledger'])
        require(set(snapshot['functions'] or {}) == FUNCTIONS, 'four 252 function fingerprints missing')
        self.complete('A', normal=new_normal, identity=new_identity, schema=snapshot)
        self.receipt['new_exit'] = self.terminate(new_app)
        self.stage = 'B-synthetic-seed'
        self.sql("INSERT INTO groups(id,name,platform) VALUES(900001,'ci-rollback-group','anthropic');"
            "INSERT INTO users(id,email,password_hash,role,status) VALUES(900001,'" + self.synthetic_email + "','" +
            self.synthetic_hash + "','admin','active');"
            "INSERT INTO accounts(id,name,platform,type,status) VALUES(900001,'disabled-ci-only','anthropic','apikey','disabled');"
            "INSERT INTO api_keys(id,user_id,key,name,group_id,status) VALUES(900001,900001,'ci-only-disabled-key','ci-key',900001,'disabled');")
        self.receipt['old_sql_roundtrip'] = self.old_sql_roundtrip(old)
        today = self.sql("SELECT (now() AT TIME ZONE 'UTC')::date")
        self.sql("UPDATE usage_group_rollup_state SET closed_before=(now() AT TIME ZONE 'UTC')::date-3,"
            "retained_from=date_trunc('day',now())-interval '30 days',timezone_name='UTC',building_until=NULL,build_dirty_from=NULL WHERE id=1")
        self.stage = 'B-old-normal-bootstrap'
        self.receipt['stages']['B']['executed'] = True
        old_app = self.app(old_image, self.prefix + '-old')
        old_normal = self.normal(old_app, OLD_VERSION)
        old_identity = self.assert_app_identity(old_app, old_image)
        self.invariant(snapshot, services)
        self.complete('B', normal=old_normal, identity=old_identity, business=self.business(old_app, 1.25))
        self.stage = 'C-old-startup-background'
        self.receipt['stages']['C']['executed'] = True
        first = self.cycle(old_app, today, 1.25)
        self.complete('C', cycle=first)
        self.stage = 'D-old-periodic-background'
        self.receipt['stages']['D']['executed'] = True
        invalidated = self.query("BEGIN; UPDATE usage_logs SET actual_cost=2.75 WHERE request_id='ci-oldboot-fixture';"
            "SELECT json_build_object('closed_before',closed_before::text,'detail_cost',2.75) FROM usage_group_rollup_state WHERE id=1; COMMIT;")
        fixture_day = (dt.date.fromisoformat(today) - dt.timedelta(days=2)).isoformat()
        require(invalidated['closed_before'] <= fixture_day, 'source UPDATE did not invalidate watermark')
        second = self.cycle(old_app, today, 2.75, first)
        require(self.identity(old_app) == old_identity['container'], 'old app restarted across C/D')
        self.invariant(snapshot, services)
        self.complete('D', invalidation=invalidated, cycle=second)
        self.stage = 'F-old-graceful-shutdown'
        self.receipt['stages']['F']['executed'] = True
        shutdown = self.terminate(old_app)
        self.invariant(snapshot, services)
        self.complete('F', exit=shutdown, services_preserved=services, schema=self.snapshot())
        self.failed_candidate(new_image, old_image, version, snapshot, services, today)
        self.invalid_index(old, new, old_image, new_image, version, services)
        self.invariant(snapshot, services)
        require(all(stage['verified'] for stage in self.receipt['stages'].values()) and self.index_receipt['verified'], 'incomplete stages')
        self.receipt['verified'] = True
        self.receipt['command_counts_by_stage'] = self.command_counts

    def cleanup(self):
        errors = []
        for process in self.processes:
            if process.poll() is None:
                # 只关闭本 runner 的 docker-exec 客户端；实际 session 在 owned PG 清理时结束。
                process.kill()
                process.wait(timeout=15)
        for name in reversed(self.containers):
            found = subprocess.run(['docker','inspect','--format','{{index .Config.Labels "sub2api.ci.owner"}}',name],
                                   capture_output=True,timeout=15)
            if found.returncode == 0:
                if found.stdout.decode().strip() != self.owner:
                    errors.append('container owner mismatch')
                    continue
                result = subprocess.run(['docker','rm','-f',name], capture_output=True,timeout=30)
                if result.returncode:
                    errors.append('owned container cleanup failed')
        found = subprocess.run(['docker','network','inspect','--format','{{index .Labels "sub2api.ci.owner"}}',self.network],
                               capture_output=True,timeout=15)
        if found.returncode == 0:
            if found.stdout.decode().strip() == self.owner:
                if subprocess.run(['docker','network','rm',self.network],capture_output=True,timeout=30).returncode:
                    errors.append('owned network cleanup failed')
            else:
                errors.append('network owner mismatch')
        for volume in reversed(self.volumes):
            found = subprocess.run(['docker','volume','inspect','--format','{{index .Labels "sub2api.ci.owner"}}',volume],
                                   capture_output=True,timeout=15)
            if found.returncode == 0:
                if found.stdout.decode().strip() != self.owner:
                    errors.append('volume owner mismatch')
                elif subprocess.run(['docker','volume','rm',volume],capture_output=True,timeout=30).returncode:
                    errors.append('owned volume cleanup failed')
        # app bind 被 uid1000 接管；只取回本次 mkdtemp 路径，不接触任何共享目录。
        for directory in (self.data, self.work / 'index-data'):
            if directory.exists():
                result = subprocess.run(['docker','run','--rm','--network','none','--label','sub2api.ci.owner='+self.owner,
                    '-v',str(directory)+':/owned','--entrypoint','chown',PG_IMAGE,'-R',f'{os.getuid()}:{os.getgid()}','/owned'],
                    capture_output=True,timeout=60)
                if result.returncode:
                    errors.append('owned data reclaim failed')
        for image in self.images:
            found = subprocess.run(['docker','image','inspect','--format','{{index .Config.Labels "sub2api.ci.owner"}}',image],
                                   capture_output=True,timeout=15)
            if found.returncode == 0 and found.stdout.decode().strip() == self.owner:
                if subprocess.run(['docker','image','rm','--no-prune',image],capture_output=True,timeout=30).returncode:
                    errors.append('owned image cleanup failed')
        if not errors:
            shutil.rmtree(self.work)
        require(not errors, 'owned resource cleanup incomplete')
        self.receipt['owned_disposable_cleanup_verified'] = True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--artifacts', required=True)
    parser.add_argument('--execute-isolated-ci-probe', action='store_true', required=True)
    args = parser.parse_args()
    require(os.environ.get('CI') == 'true' and os.environ.get('GITHUB_ACTIONS') == 'true' and
            sys.platform == 'linux', 'refusing old-app probe outside isolated Linux Actions CI')
    artifacts = Path(args.artifacts).resolve()
    artifacts.mkdir(parents=True, exist_ok=True)
    probe = Probe(Path(__file__).resolve().parents[2], artifacts)
    probe.persist()
    try:
        probe.execute()
    except Exception as error:
        probe.receipt.update(verified=False, failure_stage=probe.stage, failure_class=type(error).__name__)
        if probe.stage.startswith('invalid-index'):
            probe.index_receipt.update(verified=False,recovery_tested=False,failure_stage=probe.stage,failure_class=type(error).__name__)
        print('Old-app CI probe failed at ' + probe.stage, file=sys.stderr)
    finally:
        try:
            probe.cleanup()
        except Exception as error:
            probe.receipt.update(verified=False,cleanup_failed=True,cleanup_failure_class=type(error).__name__)
            probe.index_receipt['verified'] = False
        probe.receipt['finished_at'] = dt.datetime.now(dt.timezone.utc).isoformat()
        probe.receipt['exit_code'] = 0 if probe.receipt['verified'] else 1
        probe.receipt['command_counts_by_stage'] = probe.command_counts
        probe.index_receipt['exit_code'] = 0 if probe.index_receipt['verified'] else 1
        probe.index_receipt['commands'] = [command for command in probe.receipt['commands']
                                           if command['stage'].startswith('invalid-index')]
        probe.persist()
    return 0 if probe.receipt['verified'] else 1


if __name__ == '__main__':
    sys.exit(main())
