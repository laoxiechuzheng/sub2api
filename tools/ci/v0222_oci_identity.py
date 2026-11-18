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
import tarfile

DOCKER_MANIFEST = 'application/vnd.docker.distribution.manifest.v2+json'
OCI_MANIFEST = 'application/vnd.oci.image.manifest.v1+json'
MANIFEST_TYPES = {
    DOCKER_MANIFEST: 'application/vnd.docker.container.image.v1+json',
    OCI_MANIFEST: 'application/vnd.oci.image.config.v1+json',
}
LAYER_TYPES = {
    DOCKER_MANIFEST: {'application/vnd.docker.image.rootfs.diff.tar.gzip'},
    OCI_MANIFEST: {'application/vnd.oci.image.layer.v1.tar', 'application/vnd.oci.image.layer.v1.tar+gzip',
                   'application/vnd.oci.image.layer.v1.tar+zstd'},
}
DIGEST = re.compile(r'sha256:[0-9a-f]{64}\Z')
MAX_JSON = 4 * 1024 * 1024
OCI_LAYOUT_INDEX = 'application/vnd.oci.image.index.v1+json'
IMAGE_NAME_ANNOTATION = 'io.containerd.image.name'
REF_NAME_ANNOTATION = 'org.opencontainers.image.ref.name'


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

    def invalid_constant(_):
        raise ValueError('non-JSON numeric constant')

    require(len(data) <= MAX_JSON, 'metadata exceeds size bound')
    if isinstance(data, bytes):
        data = data.decode('utf-8')
    return json.loads(data, object_pairs_hook=pairs, parse_constant=invalid_constant)


def read_bounded(path):
    with Path(path).open('rb') as file:
        data = file.read(MAX_JSON + 1)
    require(len(data) <= MAX_JSON, 'input exceeds size bound')
    return data


def sha_digest(raw):
    return 'sha256:' + hashlib.sha256(raw).hexdigest()


def annotations_of(descriptor):
    annotations = descriptor.get('annotations', {})
    require(isinstance(annotations, dict) and all(isinstance(v, str) for v in annotations.values()),
            'invalid descriptor annotations')
    return annotations


def build_descriptor(metadata):
    require(isinstance(metadata, dict), 'BuildKit metadata must be an object')
    manifest = digest(metadata.get('containerimage.digest'), 'BuildKit manifest digest')
    descriptor = metadata.get('containerimage.descriptor')
    require(isinstance(descriptor, dict), 'missing BuildKit image descriptor')
    require(descriptor.get('mediaType') in MANIFEST_TYPES, 'unsupported BuildKit index/mediaType')
    require(descriptor.get('digest') == manifest, 'BuildKit descriptor digest mismatch')
    require(type(descriptor.get('size')) is int and 0 < descriptor['size'] <= MAX_JSON,
            'invalid BuildKit descriptor size')
    require('data' not in descriptor and 'urls' not in descriptor, 'non-local BuildKit descriptor')
    annotations_of(descriptor)
    return descriptor


def manifest_document(raw, expected_manifest):
    require(sha_digest(raw) == digest(expected_manifest, 'manifest digest'), 'raw manifest hash mismatch')
    manifest = strict_json(raw)
    require(isinstance(manifest, dict) and type(manifest.get('schemaVersion')) is int
            and manifest['schemaVersion'] == 2, 'unsupported manifest schema')
    media_type = manifest.get('mediaType')
    require(media_type in MANIFEST_TYPES and not {'manifests', 'artifactType', 'subject'} & manifest.keys(),
            'unsupported manifest index/mediaType')
    config = manifest.get('config')
    require(isinstance(config, dict), 'missing config descriptor')
    require(config.get('mediaType') == MANIFEST_TYPES[media_type], 'wrong config mediaType')
    config_digest = digest(config.get('digest'), 'manifest config digest')
    require(config_digest != expected_manifest, 'manifest M must not be config C')
    require(type(config.get('size')) is int and config['size'] > 0, 'invalid config size')
    require('urls' not in config and 'data' not in config, 'non-local config descriptor')
    layers = manifest.get('layers')
    require(isinstance(layers, list) and 0 < len(layers) <= 512, 'invalid layer descriptor list')
    for layer in layers:
        require(isinstance(layer, dict), 'invalid layer descriptor')
        layer_digest = digest(layer.get('digest'), 'layer digest')
        require(layer_digest not in (expected_manifest, config_digest), 'image descriptor role collision')
        require(layer.get('mediaType') in LAYER_TYPES[media_type], 'unsupported layer mediaType')
        require(type(layer.get('size')) is int and layer['size'] > 0, 'invalid layer size')
        require('urls' not in layer and 'data' not in layer, 'non-local layer descriptor')
    return manifest


def build_manifest(descriptor, raw):
    require(len(raw) == descriptor['size'], 'BuildKit raw manifest size mismatch')
    document = manifest_document(raw, descriptor['digest'])
    require(document['mediaType'] == descriptor['mediaType'], 'BuildKit raw manifest mediaType mismatch')
    return document


def corroborate_config(metadata, descriptor, config):
    require(config != descriptor['digest'], 'manifest M must not be config C')
    if 'containerimage.config.digest' in metadata:
        require(metadata['containerimage.config.digest'] == config, 'BuildKit config digest mismatch')
    annotations = annotations_of(descriptor)
    if 'config.digest' in annotations:
        require(annotations['config.digest'] == config, 'BuildKit config annotation mismatch')


def validated_build_manifest(metadata, raw):
    descriptor = build_descriptor(metadata)
    document = build_manifest(descriptor, raw)
    corroborate_config(metadata, descriptor, document['config']['digest'])
    return document


def blob_path(digest_value):
    return 'blobs/sha256/' + digest_value[7:]


def docker_archive_manifest(metadata, path, refs):
    """验证同一份 Docker load 输入；不提取 tar，也不打开 config/层成员。"""
    descriptor = build_descriptor(metadata)
    manifest_path = blob_path(descriptor['digest'])
    require(len(set(refs)) == 2, 'invalid expected tag set')
    # 只读未压缩的 Go tar 普通头；用 unbuffered seek 跳过 config/层字节，不走 tarfile 的 payload 预读。
    with Path(path).open('rb', buffering=0) as archive:
        members = {}
        end = os.fstat(archive.fileno()).st_size
        block = tarfile.BLOCKSIZE
        zero = bytes(block)
        while True:
            header = archive.read(block)
            if header == zero:
                require(block <= end - archive.tell() <= tarfile.RECORDSIZE and
                        archive.read(end - archive.tell()).strip(b'\0') == b'', 'invalid tar trailer')
                break
            entry = tarfile.TarInfo.frombuf(header, 'utf-8', 'strict')
            name = entry.name
            require(name not in members and len(members) < 520, 'duplicate/too many archive entries')
            if name in ('blobs', 'blobs/sha256'):
                require(entry.isdir() and entry.size == 0, 'invalid archive directory')
            else:
                require(entry.type == tarfile.REGTYPE and entry.size > 0 and
                        (name in ('oci-layout', 'index.json', 'manifest.json') or
                         re.fullmatch(r'blobs/sha256/[0-9a-f]{64}', name)), 'unexpected archive entry')
            offset = archive.tell()
            members[name] = (offset, entry.size)
            next_header = offset + ((entry.size + block - 1) // block) * block
            require(next_header + 2 * block <= end, 'truncated tar member')
            archive.seek(next_header)

        def read_member(name):
            offset, size = members.get(name, (0, 0))
            require(0 < size <= MAX_JSON, 'missing/oversize archive metadata member')
            archive.seek(offset)
            data = archive.read(size)
            require(len(data) == size, 'truncated archive metadata member')
            return data

        require(strict_json(read_member('oci-layout')) == {'imageLayoutVersion': '1.0.0'}, 'unsupported OCI layout')
        # index.json 只是 archive 的 tag 目录；所有条目都必须指向同一个镜像 descriptor，不能是索引。
        index = strict_json(read_member('index.json'))
        require(isinstance(index, dict) and set(index) == {'schemaVersion', 'mediaType', 'manifests'}
                and type(index['schemaVersion']) is int and index['schemaVersion'] == 2
                and index['mediaType'] == OCI_LAYOUT_INDEX and isinstance(index['manifests'], list)
                and len(index['manifests']) == len(refs), 'invalid archive tag directory')
        names = []
        for item in index['manifests']:
            require(isinstance(item, dict) and set(item) == {'mediaType', 'digest', 'size', 'annotations'}
                    and all(item[key] == descriptor[key] for key in ('mediaType', 'digest', 'size'))
                    and type(item['size']) is int, 'archive tag points to a different manifest descriptor')
            annotations = annotations_of(item)
            name = annotations.get(IMAGE_NAME_ANNOTATION)
            require(name in refs and annotations.get(REF_NAME_ANNOTATION) == name.rsplit(':', 1)[1],
                    'unrelated archive image name/tag annotation')
            require(all(annotations.get(key) == value for key, value in annotations_of(descriptor).items()
                        if key not in (IMAGE_NAME_ANNOTATION, REF_NAME_ANNOTATION)),
                    'archive descriptor annotation mismatch')
            names.append(name)
        require(set(names) == set(refs), 'archive tags do not own the same manifest')

        # 先确认经典 Docker 的 Config 路径不是待打开的 manifest；两条加载路径必须一致。
        docker = strict_json(read_member('manifest.json'))
        require(isinstance(docker, list) and len(docker) == 1 and isinstance(docker[0], dict)
                and set(docker[0]) == {'RepoTags', 'Config', 'Layers'}, 'invalid Docker load manifest')
        tags = docker[0]['RepoTags']
        require(isinstance(tags, list) and len(tags) == len(refs) and all(isinstance(tag, str) for tag in tags)
                and set(tags) == set(refs) and docker[0]['Config'] != manifest_path
                and isinstance(docker[0]['Layers'], list) and manifest_path not in docker[0]['Layers'],
                'unrelated Docker load tags or descriptor role collision')

        raw = read_member(manifest_path)
        document = build_manifest(descriptor, raw)
        config, layers = document['config']['digest'], document['layers']
        corroborate_config(metadata, descriptor, config)
        for item in index['manifests']:
            corroborate_config({}, item, config)
        require(docker[0]['Config'] == blob_path(config)
                and docker[0]['Layers'] == [blob_path(layer['digest']) for layer in layers],
                'Docker load manifest does not own config/layers')
        for item in [document['config'], *layers]:
            require(members.get(blob_path(item['digest']), (0, 0))[1] == item['size'],
                    'archive descriptor member missing/size mismatch')
        require(set(members) - {'blobs', 'blobs/sha256', 'oci-layout', 'index.json', 'manifest.json', manifest_path}
                == {blob_path(item['digest']) for item in [document['config'], *layers]}, 'unrelated archive blob')
    return raw


def validate_local_identity(local, expected):
    digest(local.get('local_id'), 'Docker local ID')
    require(local.get('platform') == 'linux/amd64', 'unsupported local platform')
    for field in ('source', 'revision', 'version'):
        require(isinstance(expected.get(field), str) and expected[field], f'missing expected {field}')
        require(local.get(field) == expected[field], f'local {field} mismatch')
    require(re.fullmatch(r'[0-9a-f]{40}', expected['revision']), 'invalid source revision')


def registry_link(raw, expected_manifest, expected_config, build=None):
    manifest = manifest_document(raw, expected_manifest)
    require(manifest['config']['digest'] == digest(expected_config, 'canonical config digest'),
            'registry manifest/config mismatch')
    if build is not None:
        require(manifest['config']['size'] == build['config']['size'] and len(manifest['layers']) == len(build['layers']),
                'registry config size or layer count differs from build')
        if manifest['mediaType'] == build['mediaType']:
            require([(layer['mediaType'], layer['digest'], layer['size']) for layer in manifest['layers']] ==
                    [(layer['mediaType'], layer['digest'], layer['size']) for layer in build['layers']],
                    'same-format registry layers differ from build')
        else:
            # 经典 Docker 推送可将 OCI 转成 Schema 2，并重新压缩各层；C 及其大小必须不变。
            require(build['mediaType'] == OCI_MANIFEST and manifest['mediaType'] == DOCKER_MANIFEST,
                    'unsupported registry manifest conversion')
    return {'manifest_digest': expected_manifest, 'config_digest': expected_config,
            'manifest_size': len(raw), 'manifest_media_type': manifest['mediaType'],
            'layer_count': len(manifest['layers'])}


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
    exported = commands.add_parser('export')
    local = commands.add_parser('local')
    remote = commands.add_parser('registry')
    for command in (exported, local, remote):
        command.add_argument('--metadata-env', required=True)
        command.add_argument('--build-manifest', required=True)
        command.add_argument('--receipt', required=True)
    for field in ('archive', 'version-ref', 'latest-ref'):
        exported.add_argument('--' + field, required=True)
    for command in (local, remote):
        for field in ('local-id', 'latest-id', 'expected-source', 'expected-revision',
                      'expected-version', 'binary-sha256'):
            command.add_argument('--' + field, required=True)
    for field in ('platform', 'source', 'revision', 'version'):
        local.add_argument('--' + field, required=True)
    for field in ('manifest', 'version-manifest', 'latest-manifest', 'expected-digest', 'build-receipt',
                  'asset-manifest', 'asset-manifest-sha256', 'asset-directory'):
        remote.add_argument('--' + field, required=True)
    remote.add_argument('--asset-count', type=int, required=True)
    args = parser.parse_args()
    github_sha = os.environ.get('GITHUB_SHA', '')
    context = {'github_sha': github_sha if re.fullmatch(r'[0-9a-f]{40}', github_sha) else None,
               'runner_version': 'v0222-ci-v8.1', 'command': 'v0222_oci_identity.py ' + args.command}
    failure_class = 'build_metadata'
    try:
        metadata_text = os.environ.get(args.metadata_env, '')
        metadata = strict_json(metadata_text)
        descriptor = build_descriptor(metadata)
        if args.command == 'export':
            failure_class = 'docker_archive'
            raw = docker_archive_manifest(metadata, args.archive, (args.version_ref, args.latest_ref))
            failure_class = 'evidence_write'
            Path(args.build_manifest).write_bytes(raw)
            return

        failure_class = 'build_manifest'
        raw = read_bounded(args.build_manifest)
        build_document = validated_build_manifest(metadata, raw)
        config = build_document['config']['digest']
        binding = {'config_digest': config, 'buildkit_manifest_digest': descriptor['digest'],
                   'buildkit_media_type': descriptor['mediaType'], 'buildkit_manifest_size': descriptor['size'],
                   'metadata_sha256': hashlib.sha256(metadata_text.encode()).hexdigest()}
        expected = {key: getattr(args, 'expected_' + key) for key in ('source', 'revision', 'version')}
        if args.command == 'local':
            failure_class = 'local_identity'
            fields = {key: getattr(args, key) for key in ('source', 'revision', 'version', 'platform', 'local_id')}
            validate_local_identity(fields, expected)
            require(args.local_id == args.latest_id, 'local ID differs between tags')
            require(re.fullmatch(r'[0-9a-f]{64}', args.binary_sha256), 'invalid binary hash')
            failure_class = 'evidence_write'
            write_json(args.receipt, {**fields, **binding, 'schema_version': 1,
                                      'binary_sha256': args.binary_sha256})
            print(config)
        else:
            failure_class = 'build_receipt'
            build_raw = read_bounded(args.build_receipt)
            build = strict_json(build_raw)
            require(isinstance(build, dict) and all(build.get(k) == v for k, v in binding.items()),
                    'build receipt no longer matches byte-bound BuildKit metadata')
            validate_build_receipt(build, expected, args.local_id, args.latest_id, args.binary_sha256)
            failure_class = 'registry_manifest'
            registry = read_bounded(args.manifest)
            require(registry == read_bounded(args.version_manifest) == read_bounded(args.latest_manifest),
                    'registry tags and digest reference returned different manifest bytes')
            proof = registry_link(registry, args.expected_digest, config, build_document)
            failure_class = 'asset_proof'
            assets = assets_link(args.asset_manifest, args.asset_manifest_sha256,
                                 args.asset_count, args.asset_directory)
            failure_class = 'evidence_write'
            write_json(args.receipt, {**build, **context, 'registry': proof, 'assets': assets, 'exit_code': 0,
                'build_receipt_sha256': hashlib.sha256(build_raw).hexdigest(), 'verified': True})
            print('Verified raw registry manifest -> canonical BuildKit config')
    except (ValueError, OSError, TypeError, KeyError, RecursionError, tarfile.TarError):
        # 失败类型只取受控常量，不回显输入、异常文字或 metadata/Env。
        try:
            write_json(args.receipt + '.failure.json', {**context, 'verified': False, 'exit_code': 1,
                                                        'failure_class': failure_class})
        except OSError:
            pass
        parser.exit(1, 'OCI identity verification failed; no config blob was read\n')


if __name__ == '__main__':
    main()
