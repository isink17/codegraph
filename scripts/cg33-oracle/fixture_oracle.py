"""Source-grounded Composer PSR-4 fixture oracle; production autoload policy."""
from pathlib import Path
import json, os, sqlite3, subprocess
root=Path(os.environ['CG33_WORK'])
base=root/'fixture-work'
cls='App\\Special\\Service'
call='<?php\nnamespace App;\nclass Caller { public static function go(): void { \\App\\Special\\Service::run(); } }\n'
good='<?php\nnamespace App\\Special;\nclass Service { public static function run(): void {} }\n'
wrong='<?php\nnamespace App\\Special;\nclass Other { public static function run(): void {} }\n'
cases={
 'shorter_fallback': ({'autoload':{'psr-4':{'App\\':['first','second'],'App\\Special\\':'missing'}}}, {'first/Special/Service.php':good,'second/Special/Service.php':good},'first/Special/Service.php'),
 'root_order': ({'autoload':{'psr-4':{'App\\':['first','second']}}}, {'first/Special/Service.php':good,'second/Special/Service.php':good},'first/Special/Service.php'),
 'physical_unindexed': ({'autoload':{'psr-4':{'App\\':'src','App\\Special\\':'vendor/long'}}}, {'src/Special/Service.php':good,'vendor/long/Service.php':good,'dup/Service.php':good},None),
 'existing_wrong_class': ({'autoload':{'psr-4':{'App\\':'src','App\\Special\\':'long'}}}, {'src/Special/Service.php':good,'long/Service.php':wrong,'dup/Service.php':good},None),
 'empty_prefix': ({'autoload':{'psr-4':{'':'fallback'}}}, {'fallback/App/Special/Service.php':good,'dup/Service.php':good},'fallback/App/Special/Service.php'),
 'dev_collision': ({'autoload':{'psr-4':{'App\\':'src'}},'autoload-dev':{'psr-4':{'App\\':'tests'}}}, {'src/Special/Service.php':good,'tests/Special/Service.php':good},'src/Special/Service.php'),
 'dev_only': ({'autoload-dev':{'psr-4':{'App\\':'tests'}}}, {'tests/Special/Service.php':good,'dup/Service.php':good},None),
 'malformed_prefix': ({'autoload':{'psr-4':{'App':'src'}}}, {'src/Special/Service.php':good,'dup/Service.php':good},None),
 'unmapped': ({'autoload':{'psr-4':{'Other\\':'other'}}}, {'src/Special/Service.php':good,'dup/Service.php':good},None),
}
def composer_path(manifest, folder):
    maps=manifest.get('autoload',{}).get('psr-4',{})
    if not isinstance(maps,dict) or any(p and not p.endswith('\\') for p in maps): return None
    for prefix in sorted((p for p in maps if cls.startswith(p)),key=lambda p:(-len(p),p)):
        dirs=maps[prefix]
        if isinstance(dirs,str): dirs=[dirs]
        if not isinstance(dirs,list) or any(not isinstance(d,str) for d in dirs): return None
        for d in dirs:
            suffix=cls[len(prefix):].replace('\\','/')+'.php'
            candidate=os.path.normpath(os.path.join(d,suffix))
            if (folder/candidate).is_file(): return candidate
    return None
results=[]
for name,(manifest,files,expected) in cases.items():
    folder=base/name; folder.mkdir(parents=True,exist_ok=True)
    for rel,content in {'src/Caller.php':call,**files}.items():
        p=folder/rel; p.parent.mkdir(parents=True,exist_ok=True); p.write_text(content)
    (folder/'composer.json').write_text(json.dumps(manifest,indent=2))
    if name=='physical_unindexed': (folder/'.codegraphignore').write_text('vendor/**\n')
    independent=composer_path(manifest,folder)
    # An existing physical target can be excluded from indexing or declare wrong class.
    if independent!=expected and not (name in ('physical_unindexed','existing_wrong_class') and expected is None):
        raise AssertionError((name,independent,expected))
    with (root/'results'/f'fixture-{name}.index.json').open('w') as output:
        subprocess.run([os.environ['CODEGRAPH_BIN'],'index',str(folder)],stdout=output,stderr=subprocess.STDOUT,check=True,env={**os.environ,'CODEGRAPH_HOME':str(root/'home'),'CODEGRAPH_NO_UPDATE_CHECK':'1'},timeout=90)
    con=sqlite3.connect(folder/'.codegraph/codegraph.v2.sqlite')
    rows=con.execute("""select e.dst_name,df.path,e.resolution_strategy from edges e join files sf on sf.id=e.file_id left join symbols d on d.id=e.dst_symbol_id left join files df on df.id=d.file_id where sf.path='src/Caller.php' and e.dst_name like '%Service::run%'""").fetchall()
    if len(rows)!=1: raise AssertionError((name,rows))
    got=rows[0][1]
    ok=got==expected
    results.append((name,independent,expected,got,rows[0][2],ok))
    print('\t'.join(map(str,results[-1])))
assert all(r[-1] for r in results)
print('AGREE',len(results),'/',len(results))
