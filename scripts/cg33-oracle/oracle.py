"""Independent PHP source-truth oracle for CodeGraph PHP call edges.

Reads PHP source text and composer.json only (never CodeGraph facts) to decide
what a sampled call spelling names under PHP name-resolution rules and the
Composer ClassLoader PSR-4 algorithm (Composer src/Composer/Autoload/
ClassLoader.php findFile/findFileWithExtension: classmap first, then PSR-4
longest namespace prefix first, dirs in order, falling back to shorter
prefixes). The CodeGraph DB is used only to enumerate and sample edges.

usage: oracle.py <corpus> <db> <mode:bound|refused> <n> [strategy]
"""
import os,re,sys,json,sqlite3,hashlib,collections
corpus,db,mode,n=sys.argv[1],sys.argv[2],sys.argv[3],int(sys.argv[4])
strat=sys.argv[5] if len(sys.argv)>5 else None
name=os.path.basename(os.path.abspath(corpus))

def strip(src):
    # blank out comments and string literals, preserving offsets/newlines
    out=list(src); i=0; L=len(src)
    def blank(a,b):
        for k in range(a,b):
            if out[k]!='\n': out[k]=' '
    while i<L:
        c=src[i]
        if src.startswith('//',i) or c=='#' and not src.startswith('#[',i):
            j=src.find('\n',i); j=L if j<0 else j; blank(i,j); i=j
        elif src.startswith('/*',i):
            j=src.find('*/',i+2); j=L if j<0 else j+2; blank(i,j); i=j
        elif c in '\'"':
            j=i+1
            while j<L and src[j]!=c:
                j+=2 if src[j]=='\\' else 1
            blank(i+1,min(j,L)); i=j+1
        elif src.startswith('<<<',i):
            m=re.match(r"<<<[ \t]*['\"]?([A-Za-z_]\w*)['\"]?\n",src[i:])
            if not m: i+=3; continue
            e=re.compile(r'\n[ \t]*'+m.group(1)+r'\b').search(src,i+m.end()-1)
            j=L if not e else e.start(); blank(i+m.end(),j); i=j+1
        else: i+=1
    return ''.join(out)

NS=re.compile(r'(?m)^\s*namespace\s+([A-Za-z_\\][\w\\]*)?\s*([;{])')
DECL=re.compile(r'(?m)^\s*(?:(?:abstract|final|readonly)\s+)*(class|interface|trait|enum)\s+([A-Za-z_]\w*)')
USE=re.compile(r'(?m)^\s*use\s+(?!function\b|const\b)([^;]+);')
cache={}
def parse(rel):
    if rel in cache: return cache[rel]
    raw=open(os.path.join(corpus,rel),errors='replace').read(); s=strip(raw)
    lines=[0]
    for m in re.finditer('\n',s): lines.append(m.end())
    ln=lambda off: __import__('bisect').bisect_right(lines,off)
    nss=[(m.start(),(m.group(1) or '').strip('\\')) for m in NS.finditer(s)]
    def ns_at(off):
        cur=''
        for p,v in nss:
            if p<=off: cur=v
        return cur
    decls=[]
    for m in DECL.finditer(s):
        o=s.find('{',m.end())
        d=0;j=o
        while j<len(s):
            if s[j]=='{': d+=1
            elif s[j]=='}':
                d-=1
                if d==0: break
            j+=1
        decls.append(dict(kind=m.group(1),name=m.group(2),ns=ns_at(m.start()),start=ln(m.start()),end=ln(j),a=o,b=j,
                          fq=(ns_at(m.start())+'\\'+m.group(2)).strip('\\')))
    uses=[]
    for m in USE.finditer(s):
        # skip trait-use inside class bodies
        if any(d['a']<m.start()<d['b'] for d in decls): continue
        ns=ns_at(m.start()); body=' '.join(m.group(1).split())
        g=re.match(r'([\w\\]+)\\\{(.*)\}$',body)
        items=[(g.group(1)+'\\'+x.strip()) for x in g.group(2).split(',') if x.strip()] if g else [x.strip() for x in body.split(',')]
        for it in items:
            it=re.sub(r'^(function|const)\s+','',it) if not g else it
            a=re.match(r'\\?([\w\\]+)(?:\s+as\s+(\w+))?$',it)
            if a: uses.append((ns,(a.group(2) or a.group(1).split('\\')[-1]),a.group(1)))
    r=dict(raw=raw,rawlines=raw.split('\n'),s=s,lines=lines,ln=ln,ns_at=ns_at,decls=decls,uses=uses); cache[rel]=r; return r

# corpus-wide FQCN index (case-insensitive, as PHP class names are)
allfiles=[]
for dp,dn,fn in os.walk(corpus):
    # mirror CodeGraph's walk: root-level vendor/ only (DefaultExcludes vendor/**)
    dn[:]=[x for x in dn if x not in ('.git','node_modules','.codegraph') and not (x=='vendor' and os.path.samefile(dp,corpus))]
    allfiles+=[os.path.relpath(os.path.join(dp,f),corpus) for f in fn if f.endswith('.php')]
decl_index=collections.defaultdict(list)
for rel in sorted(allfiles):
    for d in parse(rel)['decls']: decl_index[d['fq'].lower()].append((rel,d))

cj=json.load(open(os.path.join(corpus,'composer.json')))
def psr4(dev):
    m=collections.OrderedDict()
    for sec in (['autoload','autoload-dev'] if dev else ['autoload']):
        for p,dirs in (cj.get(sec,{}).get('psr-4') or {}).items():
            m.setdefault(p,[]).extend([dirs] if isinstance(dirs,str) else dirs)
    cm=[]
    for sec in (['autoload','autoload-dev'] if dev else ['autoload']): cm+=cj.get(sec,{}).get('classmap') or []
    return m,cm
def composer_file(fq,dev):
    m,cm=psr4(dev)
    for rel,d in decl_index.get(fq.lower(),[]):
        if any(rel==c or rel.startswith(c.rstrip('/')+'/') for c in cm): return rel,'classmap'
    sub=fq
    while '\\' in sub:
        sub=sub[:sub.rfind('\\')]
        for dirs in [m.get(sub+'\\')] if m.get(sub+'\\') else []:
            for dr in dirs:
                f=os.path.normpath(os.path.join(dr,fq[len(sub)+1:].replace('\\','/')+'.php'))
                if os.path.isfile(os.path.join(corpus,f)):
                    # file_exists is case-insensitive on this FS; require exact case as on Linux
                    if f in set(allfiles): return f,'psr4:'+sub+'\\'
    return None,'none'

def resolve_name(spell,ns,cls,uses):
    if spell.lower()=='self': return cls['fq'] if cls else None
    if spell.lower() in ('static','parent'): return None
    if spell.startswith('\\'): return spell[1:]
    seg=spell.split('\\')
    if seg[0].lower()=='namespace': return (ns+'\\'+'\\'.join(seg[1:])).strip('\\')
    for uns,alias,full in uses:
        if uns==ns and alias.lower()==seg[0].lower(): return '\\'.join([full]+seg[1:])
    return (ns+'\\'+spell).strip('\\')

mcache={}
def methods(rel,d):
    k=(rel,d['a'])
    if k not in mcache: mcache[k]=_methods(rel,d)
    return mcache[k]
def _methods(rel,d):
    body=parse(rel)['s'][d['a']:d['b']]
    out={}
    for m in re.finditer(r'((?:(?:public|protected|private|static|final|abstract)\s+)*)function\s+&?\s*(\w+)\s*\(',body):
        mods=m.group(1).split(); depth=body[:m.start()].count('{')-body[:m.start()].count('}')
        if depth!=1: continue
        ln=parse(rel)['ln'](d['a']+m.start()); rl=parse(rel)['rawlines']
        while ln>1 and rl[ln-2].strip().startswith('#['): ln-=1  # symbol span starts at its attributes
        out[m.group(2).lower()]=dict(static='static' in mods,vis=next((x for x in mods if x in('public','protected','private')),'public'),
                                    line=ln,name=m.group(2))
    return out

def prop_type(rel,d,prop):
    body=parse(rel)['s'][d['a']:d['b']]
    m=re.search(r'(?:public|protected|private|readonly|var)(?:\s+(?:readonly|static))*\s+(\??[\\\w]+)\s+\$'+prop+r'\b',body)
    return m.group(1).lstrip('?') if m else None

def truth(src_path,line,dst):
    """Returns (verdict, target(file,line) or None, note)."""
    p=parse(src_path); off=p['lines'][line-1] if line-1<len(p['lines']) else 0
    ns=p['ns_at'](off); cls=None
    for d in p['decls']:
        if d['a']<=off<=d['b']: cls=d
    if '->' in dst:
        parts=dst.split('->')
        if parts[0]!='$this' or not cls: return 'undetermined',None,'receiver'
        if cls['kind']=='trait': return 'undetermined',None,'trait $this'
        if len(parts)==2: fq,meth,want_static=cls['fq'],parts[1],False
        else:
            t=prop_type(src_path,cls,parts[1])
            if not t: return 'undetermined',None,'untyped property'
            fq=resolve_name(t,cls['ns'],cls,p['uses']); meth,want_static=parts[2],False
    else:
        spell,meth=dst.split('::',1); fq=resolve_name(spell,ns,cls,p['uses']); want_static=True
        if fq is None: return 'undetermined',None,spell+' keyword'
    ds=decl_index.get(fq.lower(),[])
    if not ds: return 'external',None,fq+' not declared in repo'
    if dst.startswith('self::') or (len(dst.split('->'))==2):
        tf,td=src_path,cls; how='lexical'
    elif len(ds)==1: tf,td=ds[0]; how='unique'
    else:
        f1,h1=composer_file(fq,False); f2,h2=composer_file(fq,True)
        if f1!=f2: return 'mode-dependent',None,'%s nodev=%s dev=%s'%(fq,f1,f2)
        c=[d for r,d in ds if r==f1]
        if len(c)!=1: return 'ambiguous',None,'%s %d decls in autoload file %s'%(fq,len(c),f1)
        tf,td,how=f1,c[0],h1
    ms=methods(tf,td)
    mm=ms.get(meth.lower())
    if not mm: return 'not-declared',None,'%s::%s not declared in %s (inherited/magic?)'%(fq,meth,tf)
    if mm['static']!=want_static: return 'static-mismatch',(tf,mm['line']),''
    same=cls is not None and cls['fq'].lower()==fq.lower()
    if not same and mm['vis']!='public': return 'not-visible',(tf,mm['line']),mm['vis']
    if mm['name']!=meth: how+=' case-differs(%s)'%mm['name']
    if '->' in dst and len(dst.split('->'))==3 and prop_type(src_path,cls,dst.split('->')[1]) and re.search(r'\?\s*[\\\w]+\s+\$'+dst.split('->')[1]+r'\b',parse(src_path)['s'][cls['a']:cls['b']]): how+=' nullable-property'
    return 'target',(tf,mm['line']),how

con=sqlite3.connect(db)
q='''SELECT sf.path,e.line,e.dst_name,COALESCE(s.qualified_name,''),e.resolution_strategy,df.path,d.start_line,d.qualified_name,e.evidence
FROM edges e JOIN files sf ON sf.id=e.file_id LEFT JOIN symbols s ON s.id=e.src_symbol_id
LEFT JOIN symbols d ON d.id=e.dst_symbol_id LEFT JOIN files df ON df.id=d.file_id
WHERE sf.language='php' AND sf.is_deleted=0 AND '''
if mode=='bound': q+='e.dst_symbol_id IS NOT NULL AND e.resolution_strategy=?'; args=[strat]
else: q+="e.dst_symbol_id IS NULL AND ((instr(e.dst_name,'::')>0 AND instr(e.dst_name,'->')=0) OR e.dst_name LIKE '$this->%')"; args=[]
rows=con.execute(q,args).fetchall()
key=lambda r:hashlib.sha256(('cg33-sample-v1:%s:%s:%s:%s:%s:%s'%(name,mode,strat,r[0],r[1],r[2])).encode()).hexdigest()
rows.sort(key=key)
tally=collections.Counter(); shown=0
for r in (rows if n<=0 else rows[:n]):
    src_path,line,dst,srcq,st,dpath,dline,dq,ev=r
    if mode!='bound' and ('(' in dst or '\n' in dst or dst.count('->')>2):
        tally['REFUSE-OK:chain']+=1; continue
    try: v,t,note=truth(src_path,line,dst)
    except Exception as ex: v,t,note='oracle-error',None,repr(ex)
    if mode=='bound':
        ok = v=='target' and t==(dpath,dline)
        verdict='AGREE' if ok else 'CHECK'
    else:
        verdict={'target':'MISS'}.get(v,'REFUSE-OK:'+v)
        if verdict=='MISS':
            p=parse(src_path); off=p['lines'][line-1]
            encl=[d for d in p['decls'] if d['a']<=off<=d['b']]
            if ev.endswith('nested_executable_scope'): verdict='BYDESIGN:nested-closure'
            elif encl and len(decl_index.get(encl[-1]['fq'].lower(),[]))>1: verdict='BYDESIGN:ambiguous-caller-type'
            elif 'case-differs' in note: verdict='MISS-CONSERVATIVE:case-insensitive-name'
            elif 'nullable-property' in note: verdict='MISS-CONSERVATIVE:nullable-typed-property'
            elif '?->' in dst: verdict='MISS-CONSERVATIVE:nullsafe-call'
    tally[verdict.split(':')[0] if mode=='bound' else verdict]+=1
    print('%s\t%s:%d\t%s\tgraph=%s\ttruth=%s %s %s'%(verdict,src_path,line,dst,('%s:%s %s'%(dpath,dline,dq)) if dpath else '-',v,('%s:%d'%t) if t else '-',note))
print('#',name,mode,strat,'population=%d sampled=%d'%(len(rows),len(rows) if n<=0 else min(n,len(rows))),dict(tally))
