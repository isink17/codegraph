#!/bin/sh
# Semantic projection digest of one CodeGraph DB: no row ids, only content
# identities (paths, qualified names, lines, strategies). Prints per-section
# sha256 and a total. Usage: digest.sh <db>
D="$1"
sym="SELECT f.path,s.kind,s.qualified_name,s.start_line,s.start_col,s.end_line,s.visibility,COALESCE(s.is_static,'N'),s.signature FROM symbols s JOIN files f ON f.id=s.file_id WHERE f.is_deleted=0"
sk="(SELECT f2.path||'|'||s2.kind||'|'||s2.qualified_name||'|'||s2.start_line||':'||s2.start_col FROM symbols s2 JOIN files f2 ON f2.id=s2.file_id WHERE s2.id=%s)"
src=$(printf "$sk" e.src_symbol_id); dst=$(printf "$sk" e.dst_symbol_id)
edg="SELECT f.path,e.line,e.edge_kind,e.dst_name,e.evidence,COALESCE(e.call_arity,'N'),$src,COALESCE($dst,'<unresolved>'),e.resolution_strategy,e.resolution_confidence FROM edges e JOIN files f ON f.id=e.file_id WHERE f.is_deleted=0"
ref="SELECT f.path,r.ref_kind,r.name,r.qualified_name,r.start_line,r.start_col,COALESCE($(printf "$sk" r.symbol_id),'N'),COALESCE($(printf "$sk" r.context_symbol_id),'N') FROM references_tbl r JOIN files f ON f.id=r.file_id WHERE f.is_deleted=0"
imp="SELECT f.path,i.language,i.source_specifier,i.imported_name,i.local_name,i.import_kind,i.owner_module,i.wildcard,i.is_static FROM scope_import_evidence i JOIN files f ON f.id=i.file_id WHERE f.is_deleted=0"
psr="SELECT manifest_path,mapping_role,namespace_prefix,root_path,root_ordinal FROM php_composer_psr4_mapping"
tot=""
for sec in sym edg ref imp psr; do
  eval q=\$$sec
  h=$(sqlite3 -separator '|' "$D" "$q" | LC_ALL=C sort | shasum -a 256 | cut -c1-16)
  n=$(sqlite3 "$D" "SELECT count(*) FROM ($q)")
  echo "$sec $n $h"; tot="$tot$h"
done
echo "total $(printf %s "$tot" | shasum -a 256 | cut -c1-16)"
