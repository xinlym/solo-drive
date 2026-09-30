#!/usr/bin/env python3
"""Isolated Docker integration test; requires Python 3, Docker, and Compose v2.

Build the image first: docker build -t solo-drive:0.1.0 .
Then run: python3 scripts/docker-smoke.py [--image solo-drive:0.1.0] [--port 18092]
The test binds localhost only and removes its own container, network, and volume.
It never reads production credentials or modifies production data.
"""
import argparse
from pathlib import Path
import base64, concurrent.futures, hashlib, http.cookiejar, json, os, secrets, subprocess, tempfile, time, urllib.error, urllib.request

PROJECT_ROOT = Path(__file__).resolve().parent.parent
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--image', default='solo-drive:0.1.0')
parser.add_argument('--port', type=int, default=18092)
args = parser.parse_args()
if not 1024 <= args.port <= 65535:
    parser.error('--port must be between 1024 and 65535')
base = f'http://127.0.0.1:{args.port}'
nonce = secrets.token_hex(6)
container = 'solo-drive-smoke-' + nonce
volume = container + '-data'
password = secrets.token_urlsafe(30)
jar = http.cookiejar.CookieJar()
client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
csrf = ''

def docker(*args, check=True):
    p = subprocess.run(['docker', *args], capture_output=True, text=True, cwd=PROJECT_ROOT)
    if check and p.returncode:
        raise RuntimeError('docker ' + args[0] + ' failed: ' + p.stderr[:1000])
    return p.stdout.strip()

def request(method, path, data=None, headers=None, anonymous=False):
    h = dict(headers or {})
    if csrf and not anonymous:
        h['X-CSRF-Token'] = csrf
    if isinstance(data, dict):
        data = json.dumps(data).encode()
        h['Content-Type'] = 'application/json'
    req = urllib.request.Request(path if path.startswith('http') else base + path, data=data, method=method, headers=h)
    opener = urllib.request if anonymous else client
    try:
        response = opener.urlopen(req, timeout=20) if anonymous else opener.open(req, timeout=20)
    except urllib.error.HTTPError as e:
        response = e
    with response:
        return response.status, response.headers, response.read()

def expect(code, response):
    assert response[0] == code, (code, response[0], response[2][:500])
    return response

def wait_ready():
    for _ in range(60):
        try:
            if request('GET', '/healthz', anonymous=True)[0] == 200:
                return
        except (urllib.error.URLError, ConnectionError):
            pass
        time.sleep(0.25)
    raise RuntimeError('healthcheck timeout: ' + docker('logs', container, check=False))

image_info = json.loads(docker('image', 'inspect', args.image))[0]
image_size = docker('image', 'ls', args.image, '--format', '{{.Size}}')

with tempfile.TemporaryDirectory(prefix='solodrive-docker-smoke-') as tmp:
    secret_path = os.path.join(tmp, 'password')
    with open(secret_path, 'w') as f:
        f.write(password)
    os.chmod(secret_path, 0o644)
    docker('volume', 'create', volume)
    compose_path = os.path.join(tmp, 'compose.json')
    compose = {'services': {'drive': {
        'image': args.image, 'container_name': container,
        'ports': [f'127.0.0.1:{args.port}:8091'], 'read_only': True,
        'tmpfs': ['/tmp:size=16m,mode=1777,noexec,nosuid'],
        'cap_drop': ['ALL'], 'security_opt': ['no-new-privileges:true'],
        'mem_limit': '384m', 'pids_limit': 128,
        'volumes': [volume + ':/data', secret_path + ':/run/secrets/admin_password:ro'],
        'environment': {'SOLODRIVE_PUBLIC_URL':base, 'SOLODRIVE_COOKIE_SECURE':'false', 'SOLODRIVE_RESERVE_BYTES':'0', 'GOMEMLIMIT':'192MiB'},
        'healthcheck': {'test':['CMD','/solodrive','healthcheck'], 'interval':'1s', 'timeout':'3s', 'retries':3}
    }}, 'volumes': {volume: {'external': True}}}
    with open(compose_path, 'w') as f:
        json.dump(compose, f)
    def start():
        docker('compose', '--project-name', container, '--file', compose_path, 'up', '--detach', '--no-build')
        wait_ready()
    try:
        start()
        state = json.loads(docker('inspect', container))[0]
        assert state['Config']['User'] == '10001:10001'
        assert state['HostConfig']['ReadonlyRootfs'] is True
        page = expect(200, request('GET', '/', anonymous=True))[2]
        assert b'<html' in page.lower() and len(page) > 100
        expect(401, request('GET', '/api/files', anonymous=True))
        auth = json.loads(expect(200, request('POST', '/api/login', {'username':'admin','password':password}))[2])
        csrf = auth['csrf']
        storage = json.loads(expect(200, request('GET', '/api/storage'))[2])
        data = os.urandom(16 * 1024 * 1024)
        tus = {'Tus-Resumable':'1.0.0'}
        created = expect(201, request('POST', '/api/uploads/', b'', dict(tus, **{'Upload-Length':str(len(data)), 'Upload-Metadata':'filename ' + base64.b64encode('docker-test.bin'.encode()).decode()})))
        upload_url = created[1]['Location']
        split = 4 * 1024 * 1024
        block = data[:split]
        headers = dict(tus, **{'Content-Type':'application/offset+octet-stream', 'Upload-Offset':'0', 'Upload-Checksum':'sha256 ' + base64.b64encode(hashlib.sha256(block).digest()).decode()})
        expect(204, request('PATCH', upload_url, block, headers))
        assert expect(200, request('HEAD', upload_url, headers=tus))[1]['Upload-Offset'] == str(split)
        docker('compose', '--project-name', container, '--file', compose_path, 'down', '--timeout', '35')
        for _ in range(100):
            if not json.loads(docker('inspect', container, check=False) or '[]'):
                break
            time.sleep(0.1)
        start()
        expect(200, request('GET', '/api/session'))
        assert expect(200, request('HEAD', upload_url, headers=tus))[1]['Upload-Offset'] == str(split)
        block = data[split:]
        headers = dict(tus, **{'Content-Type':'application/offset+octet-stream', 'Upload-Offset':str(split), 'Upload-Checksum':'sha256 ' + base64.b64encode(hashlib.sha256(block).digest()).decode()})
        expect(204, request('PATCH', upload_url, block, headers))
        files = json.loads(expect(200, request('GET', '/api/files'))[2])['files']
        assert len(files) == 1 and files[0]['size'] == len(data)
        file_id = files[0]['id']
        share = json.loads(expect(201, request('POST', '/api/files/' + file_id + '/shares', {'expires_in_hours':0}))[2])
        expect(200, request('GET', share['url'], anonymous=True))
        head = expect(200, request('HEAD', share['download_url'], anonymous=True))
        assert head[1]['Accept-Ranges'] == 'bytes' and int(head[1]['Content-Length']) == len(data)
        def get_range(i):
            n = len(data) // 8
            start = i * n
            end = start + n - 1
            result = expect(206, request('GET', share['download_url'], headers={'Range':f'bytes={start}-{end}'}, anonymous=True))
            assert result[1]['Content-Range'] == f'bytes {start}-{end}/{len(data)}'
            return result[2]
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            downloaded = b''.join(pool.map(get_range, range(8)))
        assert hashlib.sha256(downloaded).digest() == hashlib.sha256(data).digest()
        expect(200, request('DELETE', '/api/shares/' + share['id']))
        expect(404, request('GET', share['download_url'], anonymous=True))
        stats = docker('stats', '--no-stream', '--format', '{{.MemUsage}} / {{.CPUPerc}}', container)
        docker('exec', container, '/solodrive', 'healthcheck')
        health = ''
        for _ in range(30):
            health = json.loads(docker('inspect', container))[0]['State']['Health']['Status']
            if health == 'healthy':
                break
            time.sleep(0.5)
        assert health == 'healthy', health
        expect(200, request('DELETE', '/api/files/' + file_id))
        assert json.loads(expect(200, request('GET', '/api/files'))[2])['files'] == []
        report = {'result':'PASS', 'image':{'tag':args.image, 'id':image_info['Id'], 'size':image_size, 'engine_reported_size_bytes':image_info['Size']}, 'checks':{'nonroot_readonly':True,'home_and_share_page':True,'private_api':True,'login_and_storage':True,'persistent_session':True,'resume_after_container_recreate':True,'sha256_upload_chunks':True,'eight_parallel_ranges_sha256_match':True,'revoke_and_delete':True,'health':health,'transfer_size_bytes':len(data),'memory_after_transfer_sample':stats,'storage_fields':list(storage)}}
    finally:
        docker('compose', '--project-name', container, '--file', compose_path, 'down', '--timeout', '35', check=False)
        for _ in range(50):
            if not json.loads(docker('inspect', container, check=False) or '[]'):
                break
            time.sleep(0.1)
        docker('volume', 'rm', volume)

    report['checks']['temporary_resources_removed'] = True
    print(json.dumps(report, ensure_ascii=False))
