#!/usr/bin/env python3
"""Run the optional adapter against a temporary, pinned Docker control service."""
import argparse,json,os,re,secrets,socket,subprocess,tempfile,time,urllib.request,urllib.error
from pathlib import Path
SERVER='opensandbox/server@sha256:25bebdcf1cf7724eb923faecbdbbf82d4038c6ab35c736151574726d14006f30'
EXECD='opensandbox/execd@sha256:c75836c46ef4302afccec182d5d7339eb2f905f51e3d73b7aa17bb76c832526c'
def main():
 parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--output',type=Path);args=parser.parse_args()
 repo=Path(__file__).resolve().parents[1];output=(args.output or Path(tempfile.mkdtemp(prefix='harness-sandbox-results-'))).resolve();output.mkdir(parents=True,exist_ok=True)
 env=os.environ.copy();env.pop('GOROOT',None);env['GOTOOLCHAIN']='go1.25.0'
 name='harness-sandbox-'+secrets.token_hex(6);key=secrets.token_hex(24);created=False;port=0
 def command(argv,log=None,timeout=300,cwd=None):
  if log:
   with (output/log).open('w') as handle: return subprocess.run(argv,cwd=cwd,env=env,stdout=handle,stderr=subprocess.STDOUT,timeout=timeout,check=True)
  return subprocess.run(argv,cwd=cwd,env=env,stdout=subprocess.DEVNULL,timeout=timeout,check=True)
 def api(route,method='GET'):
  req=urllib.request.Request(f'http://127.0.0.1:{port}/v1'+route,method=method,headers={'OPEN-SANDBOX-API-KEY':key})
  with urllib.request.urlopen(req,timeout=10) as response:
   raw=response.read();return json.loads(raw) if raw else None
 with tempfile.TemporaryDirectory(prefix='harness-sandbox-') as temp:
  temp=Path(temp)
  try:
   command(['docker','version'],log='docker-version.log')
   for image in (SERVER,EXECD):
    try: command(['docker','image','inspect',image])
    except subprocess.CalledProcessError: command(['docker','pull',image],log='pull-'+image.split('/')[1].split('@')[0]+'.log')
   with socket.socket() as sock: sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
   config=f'''[server]
host="0.0.0.0"
port=8080
api_key="{key}"
max_sandbox_timeout_seconds=1800
[log]
level="INFO"
[runtime]
type="docker"
execd_image="{EXECD}"
[proxy]
resolve_internal=true
[store]
type="sqlite"
path="/tmp/harness-validation.db"
[docker]
network_mode="bridge"
port_range_min=45000
port_range_max=48000
pids_limit=256
[ingress]
mode="direct"
'''
   (temp/'server.toml').write_text(config)
   command(['docker','create','--name',name,'--cpus','1','--memory','768m','-p',f'127.0.0.1:{port}:8080','-v','/var/run/docker.sock:/var/run/docker.sock',SERVER,'--config','/tmp/server.toml']);created=True
   command(['docker','cp',str(temp/'server.toml'),name+':/tmp/server.toml']);command(['docker','start',name])
   for _ in range(100):
    try:
     with urllib.request.urlopen(f'http://127.0.0.1:{port}/health',timeout=2): break
    except OSError: time.sleep(.2)
   else: raise RuntimeError('control service did not become ready')
   # Workspace resolves this unreleased optional module dependency locally.
   (temp/'go.work').write_text('go 1.25.0\nuse (\n'+json.dumps(str(repo))+'\n'+json.dumps(str(repo/'sandbox/opensandbox'))+'\n)\nreplace github.com/kart-io/goagent-harness v0.0.0-20261101021826-231f713ad219 => '+json.dumps(str(repo))+'\n')
   env.update(GOWORK=str(temp/'go.work'),HARNESS_SANDBOX_DOMAIN=f'127.0.0.1:{port}',HARNESS_SANDBOX_API_KEY=key,HARNESS_SANDBOX_IMAGE=SERVER)
   print('Running real adapter, filesystem, cancellation and lifecycle tests...',flush=True)
   command(['go','test','-race','-v','-count=1','-timeout','240s','-run','TestRealDocker'],cwd=repo/'sandbox/opensandbox',log='real-test.log')
   ids=re.findall(r'(?:owned|independent|preset) sandbox ([a-f0-9-]+)',(output/'real-test.log').read_text())
   if len(ids)!=3: raise RuntimeError('expected all three owned sandbox IDs in evidence')
   for sandbox_id in ids:
    remaining=subprocess.check_output(['docker','ps','-aq','--filter','name=^sandbox-'+sandbox_id+'$'],text=True,timeout=15).strip()
    if remaining: raise RuntimeError('sandbox container still exists after Destroy: '+sandbox_id)
   (output/'cleanup.json').write_text(json.dumps({'verified_absent_container_ids':ids},indent=2)+'\n')
   for example in ('coding','workflow'):
    command(['go','run','./examples/'+example],cwd=repo/'sandbox/opensandbox',log='example-'+example+'.log')
   print('PASS: '+str(output),flush=True)
  finally:
   if created:
    try:
     # This server owns an independent SQLite store and is private to this run.
     items=api('/sandboxes?pageSize=100').get('items',[])
     for item in items:
      if item.get('metadata',{}).get('harness.version')=='opensandbox-1.2.0-fs1':
       try: api('/sandboxes/'+item['id'],'DELETE')
       except OSError: pass
       subprocess.run(['docker','rm','-f','sandbox-'+item['id']],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=15)
     command(['docker','logs',name],log='server.log',timeout=15)
    finally: command(['docker','rm','-f',name],timeout=15)
if __name__=='__main__': main()
