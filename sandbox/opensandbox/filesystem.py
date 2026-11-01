import os,sys,json,base64,hashlib,stat,fcntl,heapq
root,request_path=sys.argv[1:3]
maximum=int(sys.argv[3])
def dec(v): return base64.b64decode(v or '')
def enc(v): return base64.b64encode(v).decode('ascii')
class Failure(Exception):
 def __init__(self,code): self.code=code
def rooted(p,follow=True):
 if b'\0' in p: raise Failure('invalid')
 rb=os.fsencode(root)
 a=os.path.normpath(p if os.path.isabs(p) else os.path.join(rb,p))
 if os.path.commonpath([rb,a])!=rb: raise Failure('outside')
 if follow: a=os.path.realpath(a)
 elif a!=rb: a=os.path.join(os.path.realpath(os.path.dirname(a)),os.path.basename(a))
 if os.path.commonpath([rb,a])!=rb: raise Failure('outside')
 r=os.path.relpath(a,rb)
 if len(r)>4096: raise Failure('invalid')
 return r
def directory(parts,create=False):
 fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY)
 try:
  for p in parts:
   if p in (b'',b'.','','.'): continue
   if create:
    try: os.mkdir(p,0o755,dir_fd=fd)
    except FileExistsError: pass
   nxt=os.open(p,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
   os.close(fd);fd=nxt
  return fd
 except BaseException:
  os.close(fd);raise
def parent(r,create=False):
 pieces=r.split(b'/');return directory(os.fsencode(root).split(b'/')+pieces[:-1],create),pieces[-1]
def info(r,s):
 kind='regular'
 for test,name in [(stat.S_ISDIR,'dir'),(stat.S_ISLNK,'symlink'),(stat.S_ISFIFO,'fifo'),(stat.S_ISSOCK,'socket'),(stat.S_ISBLK,'block'),(stat.S_ISCHR,'char')]:
  if test(s.st_mode): kind=name;break
 return dict(path=enc(r),size=s.st_size,mode=stat.S_IMODE(s.st_mode),kind=kind,mtime=s.st_mtime_ns)
def content(fd):
 if not stat.S_ISREG(os.fstat(fd).st_mode): raise Failure('invalid')
 with os.fdopen(fd,'rb',closefd=False) as f: data=f.read(maximum+1)
 if len(data)>maximum: raise Failure('file_limit')
 return data
def execute(q):
 op=q['op']
 if op=='init':
  fd=directory(os.fsencode(root).split(b'/'),q.get('create',False));os.close(fd);return {}
 r=rooted(dec(q.get('path')),op not in ('remove','entry'))
 if op in ('write','remove'):
  # A caller authorizes a canonical mutation path. Never silently retarget it
  # after an alias changes between Resolve and the final no-follow operation.
  raw=dec(q.get('path'));rb=os.fsencode(root)
  lexical=os.path.relpath(os.path.normpath(raw if os.path.isabs(raw) else os.path.join(rb,raw)),rb)
  if lexical!=r: raise Failure('invalid')
  protected=q.get('protected_names',[])
  if any(not isinstance(n,str) or not n or '/' in n or n in ('.','..') for n in protected): raise Failure('invalid')
  if any(os.fsencode(n) in r.split(b'/') for n in protected): raise Failure('permission')

 if op=='resolve':
  # Missing ancestors are valid for a future WriteFile; actual writes
  # repeat the no-follow traversal before touching the target.
  pieces=os.fsencode(root).split(b'/')+r.split(b'/')[:-1]
  fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY)
  try:
   for part in pieces:
    if part in (b'',b'.'): continue
    try: nxt=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
    except FileNotFoundError: break
    os.close(fd);fd=nxt
  finally: os.close(fd)
  return dict(path=enc(r))
 if op=='list':
  fd=directory(os.fsencode(root).split(b'/')+r.split(b'/'))
  try:
   after=dec(q.get('after'));limit=min(64,max(1,q.get('limit',64)))
   with os.scandir(fd) as it: names=heapq.nsmallest(limit+1,(os.fsencode(e.name) for e in it if os.fsencode(e.name)>after))
   entries=[]
   for name in names[:limit]:
    p=name if r==b'.' else r+b'/'+name
    entries.append(info(p,os.stat(name,dir_fd=fd,follow_symlinks=False)))
   return dict(entries=entries,more=len(names)>limit)
  finally: os.close(fd)
 fd,name=parent(r,op=='write')
 try:
  if op in ('stat','entry'): return dict(info=info(r,os.stat(name,dir_fd=fd,follow_symlinks=False)))
  if op=='read':
   f=os.open(name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=fd)
   try: data=content(f)
   finally: os.close(f)
   return dict(data=enc(data),version=hashlib.sha256(data).hexdigest())
  if op=='remove':
   if r==b'.': raise Failure('invalid')
   s=os.stat(name,dir_fd=fd,follow_symlinks=False)
   if stat.S_ISDIR(s.st_mode): os.rmdir(name,dir_fd=fd)
   else: os.unlink(name,dir_fd=fd)
   os.fsync(fd);return {}
  if op=='write':
   data=dec(q.get('data'))
   if len(data)>maximum: raise Failure('file_limit')
   existing=None;mode=q.get('perm',0) or 0o644
   try:
    f=os.open(name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=fd)
    try:
     s=os.fstat(f);existing=content(f)
     if not q.get('perm',0): mode=stat.S_IMODE(s.st_mode)
    finally: os.close(f)
   except FileNotFoundError: pass
   if q.get('create_if_absent') and existing is not None: raise Failure('exists')
   if q.get('version') and (existing is None or hashlib.sha256(existing).hexdigest()!=q['version']): raise Failure('stale')
   tmp=os.fsencode('.harness-'+os.urandom(16).hex());version=hashlib.sha256(data).hexdigest();committed=False
   f=os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,mode&0o777,dir_fd=fd)
   try:
    with os.fdopen(f,'wb') as out:
     out.write(data);out.flush();os.fchmod(out.fileno(),mode&0o777);os.fsync(out.fileno())
    os.replace(tmp,name,src_dir_fd=fd,dst_dir_fd=fd);committed=True;os.fsync(fd)
   except OSError:
    if committed: return dict(error_code='unconfirmed',version=version)
    raise
   finally:
    if not committed:
     try: os.unlink(tmp,dir_fd=fd)
     except FileNotFoundError: pass
   return dict(version=version)
  raise Failure('invalid')
 finally: os.close(fd)
try:
 with open(request_path,'rb') as f: raw=f.read(maximum*2+65537)
 if len(raw)>maximum*2+65536: raise Failure('file_limit')
 q=json.loads(raw)
 locks='/tmp/.goagent-harness-locks-'+str(os.getuid())
 try: os.mkdir(locks,0o700)
 except FileExistsError: pass
 lfd=os.open(locks,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
 try:
  s=os.fstat(lfd)
  if s.st_uid!=os.getuid() or stat.S_IMODE(s.st_mode)!=0o700: raise Failure('permission')
  lock=os.open(hashlib.sha256(os.fsencode(root)).hexdigest(),os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600,dir_fd=lfd)
 finally: os.close(lfd)
 try:
  if not stat.S_ISREG(os.fstat(lock).st_mode): raise Failure('invalid')
  fcntl.flock(lock,fcntl.LOCK_EX);reply=execute(q)
 finally: os.close(lock)
except Failure as e: reply=dict(error_code=e.code)
except FileNotFoundError: reply=dict(error_code='not_exist')
except PermissionError: reply=dict(error_code='permission')
except OSError as e: reply=dict(error_code='io',message=str(e))
except (ValueError,KeyError,TypeError) as e: reply=dict(error_code='invalid',message=str(e))
reply['protocol']=1
print(json.dumps(reply,separators=(',',':')))
