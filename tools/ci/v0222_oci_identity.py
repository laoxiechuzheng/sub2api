"""Verify BuildKit C and exact registry M→C, without reading config blobs/Env.

Docker store-dependent .Id is an opaque local continuity identifier. It never
supplies the canonical config field in the historical release-manifest schema.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import sys

MANIFEST_TYPES = {
    'application/vnd.docker.distribution.manifest.v2+json':
        'application/vnd.docker.container.image.v1+json',
    'application/vnd.oci.image.manifest.v1+json':
        'application/vnd.oci.image.config.v1+json',
}
DIGEST = re.compile(r'sha256:[0-9a-f]{64}\Z')


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(value, name):
    require(isinstance(value, str) and DIGEST.fullmatch(value), f'invalid {name}')
    return value


def strict_json(data):
    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, 'duplicate JSON member')
            result[key] = value
        return result
    require(len(data) <= 4 * 1024 * 1024, 'metadata exceeds size bound')
    return json.loads(data, object_pairs_hook=pairs)


def build_config(metadata):
    require(isinstance(metadata, dict), 'BuildKit metadata must be an object')
    config = digest(metadata.get('containerimage.config.digest'), 'BuildKit config digest')
    manifest = digest(metadata.get('containerimage.digest'), 'BuildKit manifest digest')
    descriptor = metadata.get('containerimage.descriptor')
    require(isinstance(descriptor, dict), 'missing BuildKit image descriptor')
    require(descriptor.get('mediaType') in MANIFEST_TYPES, 'unsupported BuildKit index/mediaType')
    require(descriptor.get('digest') == manifest, 'BuildKit descriptor digest mismatch')
    require(type(descriptor.get('size')) is int and descriptor['size'] > 0,
            'invalid BuildKit descriptor size')
    annotations = descriptor.get('annotations', {})
    require(isinstance(annotations, dict), 'invalid BuildKit annotations')
    if 'config.digest' in annotations:
        require(annotations['config.digest'] == config, 'BuildKit config annotation mismatch')
    return config, manifest


def local_config_id(metadata, local, expected):
    config, _ = build_config(metadata)
    digest(local.get('local_id'), 'Docker local ID')
    require(local.get('platform') == 'linux/amd64', 'unsupported local platform')
    for field in ('source', 'revision', 'version'):
        require(isinstance(expected.get(field), str) and expected[field], f'missing expected {field}')
        require(local.get(field) == expected[field], f'local {field} mismatch')
    require(re.fullmatch(r'[0-9a-f]{40}', expected['revision']), 'invalid source revision')
    return config


def registry_link(raw, expected_manifest, expected_config):
    digest(expected_manifest, 'registry manifest digest')
    digest(expected_config, 'canonical config digest')
    require('sha256:' + hashlib.sha256(raw).hexdigest() == expected_manifest,
            'registry raw manifest hash mismatch')
    manifest = strict_json(raw)
    require(isinstance(manifest, dict) and manifest.get('schemaVersion') == 2,
            'unsupported registry schema')
    media_type = manifest.get('mediaType')
    require(media_type in MANIFEST_TYPES and 'manifests' not in manifest,
            'unsupported registry index/mediaType')
    config = manifest.get('config')
    require(isinstance(config, dict), 'missing registry config descriptor')
    require(config.get('mediaType') == MANIFEST_TYPES[media_type], 'wrong config mediaType')
    require(config.get('digest') == expected_config, 'registry manifest/config mismatch')
    require(type(config.get('size')) is int and config['size'] > 0, 'invalid config size')
    require(not config.get('urls'), 'external config descriptor is unsupported')
    layers = manifest.get('layers')
    require(isinstance(layers, list) and 0 < len(layers) <= 512, 'invalid layer descriptor list')
    for layer in layers:
        require(isinstance(layer, dict), 'invalid layer descriptor')
        digest(layer.get('digest'), 'layer digest')
        require(type(layer.get('size')) is int and layer['size'] > 0, 'invalid layer size')
        require(not layer.get('urls'), 'external layers are unsupported')
    return {'manifest_digest': expected_manifest, 'config_digest': expected_config,
            'manifest_size': len(raw), 'manifest_media_type': media_type,
            'layer_count': len(layers)}


BUILD_FIELDS = {'schema_version', 'source', 'revision', 'version', 'platform', 'local_id',
                'config_digest', 'buildkit_manifest_digest', 'buildkit_media_type',
                'buildkit_manifest_size', 'metadata_sha256', 'binary_sha256'}


def validate_build_receipt(build, expected, local_id, latest_id, binary_sha256):
    require(isinstance(build, dict) and set(build) == BUILD_FIELDS,
            'unexpected/missing build receipt fields')
    require(type(build['schema_version']) is int and build['schema_version'] == 1,
            'unsupported build receipt schema')
    for field in ('source', 'revision', 'version'):
        require(isinstance(expected.get(field), str) and bool(expected[field]) and
                build[field] == expected[field], 'build receipt source/version mismatch')
    require(re.fullmatch(r'[0-9a-f]{40}', expected['revision']), 'invalid source revision')
    require(build['platform'] == 'linux/amd64', 'build receipt platform mismatch')
    for field in ('config_digest', 'buildkit_manifest_digest', 'local_id'):
        digest(build[field], field)
    require(digest(local_id, 'current local ID') == build['local_id'] ==
            digest(latest_id, 'current latest ID'), 'local ID changed after build')
    for field in ('binary_sha256', 'metadata_sha256'):
        require(isinstance(build[field], str) and re.fullmatch(r'[0-9a-f]{64}', build[field]),
                'invalid build receipt hash')
    require(build['binary_sha256'] == binary_sha256, 'binary changed after build')
    require(build['buildkit_media_type'] in MANIFEST_TYPES and
            type(build['buildkit_manifest_size']) is int and build['buildkit_manifest_size'] > 0,
            'invalid BuildKit descriptor receipt')
    return build['config_digest']


def assets_link(path, expected_hash, expected_count, directory):
    raw = Path(path).read_bytes()
    require(hashlib.sha256(raw).hexdigest() == expected_hash, 'asset manifest hash mismatch')
    value = strict_json(raw)
    require(isinstance(value, dict) and set(value) == {'files'} and isinstance(value['files'], list),
            'invalid asset manifest')
    files = value['files']
    require((Path(directory) / 'assets').is_dir() and not (Path(directory) / 'assets').is_symlink(),
            'asset directory missing or symlink')
    require(0 < len(files) == expected_count <= 512, 'asset count mismatch')
    seen, total = set(), 0
    for item in files:
        require(isinstance(item, dict) and set(item) == {'path', 'sha256', 'size'}, 'invalid asset entry')
        name = item['path']
        require(isinstance(name, str) and re.fullmatch(r'/assets/[A-Za-z0-9._-]+', name)
                and name not in seen and '..' not in name, 'unsafe/duplicate asset path')
        require(type(item['size']) is int and 0 <= item['size'] <= 64 * 1024 * 1024,
                'invalid asset size')
        require(isinstance(item['sha256'], str) and re.fullmatch(r'[0-9a-f]{64}', item['sha256']),
                'invalid asset hash')
        actual = Path(directory) / name.lstrip('/')
        require(actual.is_file() and not actual.is_symlink(), 'asset missing or symlink')
        data = actual.read_bytes()
        require(len(data) == item['size'] and hashlib.sha256(data).hexdigest() == item['sha256'],
                'asset bytes mismatch')
        seen.add(name)
        total += len(data)
    require(total <= 64 * 1024 * 1024 and seen == {
        '/assets/' + item.name for item in (Path(directory) / 'assets').iterdir()},
        'asset file set/total size mismatch')
    return {'sha256': expected_hash, 'count': expected_count, 'total_size': total}


def write_json(path, value):
    Path(path).write_text(json.dumps(value, indent=2, sort_keys=True) + '\n', encoding='utf-8')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    local = commands.add_parser('local')
    local.add_argument('--metadata-env', required=True)
    for field in ('local-id', 'platform', 'source', 'revision', 'version',
                  'expected-source', 'expected-revision', 'expected-version', 'receipt'):
        local.add_argument('--' + field, required=True)
    remote = commands.add_parser('registry')
    remote.add_argument('--manifest', required=True)
    remote.add_argument('--expected-digest', required=True)
    remote.add_argument('--build-receipt', required=True)
    remote.add_argument('--metadata-env', required=True)
    remote.add_argument('--receipt', required=True)
    for field in ('expected-source', 'expected-revision', 'expected-version', 'local-id',
                  'latest-id', 'binary-sha256', 'asset-manifest', 'asset-manifest-sha256',
                  'asset-directory'):
        remote.add_argument('--' + field, required=True)
    remote.add_argument('--asset-count', type=int, required=True)
    local.add_argument('--binary-sha256', required=True)
    args = parser.parse_args()
    try:
        if args.command == 'local':
            metadata = strict_json(os.environ.get(args.metadata_env, ''))
            fields = {key: getattr(args, key) for key in ('source', 'revision', 'version', 'platform')}
            fields['local_id'] = args.local_id
            expected = {key: getattr(args, 'expected_' + key) for key in ('source', 'revision', 'version')}
            config = local_config_id(metadata, fields, expected)
            _, manifest = build_config(metadata)
            require(re.fullmatch(r'[0-9a-f]{64}', args.binary_sha256), 'invalid binary hash')
            write_json(args.receipt, {**fields, 'schema_version': 1, 'config_digest': config,
                'buildkit_manifest_digest': manifest, 'binary_sha256': args.binary_sha256,
                'buildkit_media_type': metadata['containerimage.descriptor']['mediaType'],
                'buildkit_manifest_size': metadata['containerimage.descriptor']['size'],
                'metadata_sha256': hashlib.sha256(os.environ[args.metadata_env].encode()).hexdigest()})
            print(config)
        else:
            build_raw = Path(args.build_receipt).read_bytes()
            build = strict_json(build_raw)
            require(isinstance(build, dict), 'build receipt must be an object')
            metadata = strict_json(os.environ.get(args.metadata_env, ''))
            config_from_metadata, manifest_from_metadata = build_config(metadata)
            require(build.get('metadata_sha256') == hashlib.sha256(os.environ[args.metadata_env].encode()).hexdigest()
                    and build.get('config_digest') == config_from_metadata
                    and build.get('buildkit_manifest_digest') == manifest_from_metadata
                    and build.get('buildkit_media_type') == metadata['containerimage.descriptor']['mediaType']
                    and build.get('buildkit_manifest_size') == metadata['containerimage.descriptor']['size'],
                    'build receipt no longer matches original BuildKit metadata')
            expected = {key: getattr(args, 'expected_' + key) for key in ('source', 'revision', 'version')}
            config = validate_build_receipt(build, expected, args.local_id, args.latest_id, args.binary_sha256)
            proof = registry_link(Path(args.manifest).read_bytes(), args.expected_digest, config)
            require(args.expected_digest != config, 'manifest M must not be config C')
            assets = assets_link(args.asset_manifest, args.asset_manifest_sha256,
                                 args.asset_count, args.asset_directory)
            write_json(args.receipt, {**build, 'registry': proof, 'assets': assets,
                'github_sha': os.environ.get('GITHUB_SHA'), 'runner_version': 'v0222-ci-v8.1',
                'command': 'v0222_oci_identity.py registry', 'exit_code': 0,
                'build_receipt_sha256': hashlib.sha256(build_raw).hexdigest(), 'verified': True})
            print('Verified raw registry manifest -> canonical BuildKit config')
    except (ValueError, OSError, TypeError, KeyError):
        # 不覆盖成功路径为假绿；失败留下独立安全状态，不回显 metadata/Env。
        try:
            write_json(args.receipt + '.failure.json', {'verified': False, 'exit_code': 1,
                'command': 'v0222_oci_identity.py ' + args.command,
                'github_sha': os.environ.get('GITHUB_SHA'), 'runner_version': 'v0222-ci-v8.1'})
        except OSError:
            pass
        parser.exit(1, 'OCI identity verification failed; no config blob was read\n')


if __name__ == '__main__':
    main()
