# Deterministic mutation set S1 over a corpus. Files ranked by
# sha256("cg33-seed-1:"+path); first 3*K PHP files (excluding vendor/.git):
# rank%3==0 delete, ==1 insert a blank line after the first line (shifts lines),
# ==2 copy to <name>.dup.php (duplicate FQCN; PSR-4 must pick the original).
import os,sys,hashlib,shutil
root,k=sys.argv[1],int(sys.argv[2])
fs=[]
for dp,dn,fn in os.walk(root):
    dn[:]=[x for x in dn if x not in ('.git','vendor','node_modules','.codegraph')]
    fs+=[os.path.relpath(os.path.join(dp,f),root) for f in fn if f.endswith('.php')]
fs.sort(key=lambda p:hashlib.sha256(('cg33-seed-1:'+p).encode()).hexdigest())
for i,p in enumerate(fs[:3*k]):
    a=os.path.join(root,p)
    if i%3==0: os.remove(a); print('delete',p)
    elif i%3==1:
        L=open(a,'rb').read().split(b'\n',1); open(a,'wb').write(L[0]+b'\n\n'+(L[1] if len(L)>1 else b'')); print('shift',p)
    else: shutil.copy(a,a[:-4]+'.dup.php'); print('dup',p)
