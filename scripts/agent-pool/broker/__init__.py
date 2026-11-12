"""O5 protocol core (CG-150 PR-1): pure, stdlib-only decision logic for the
owner-only recovery broker. Nothing here performs file, network or process I/O,
nothing imports it outside its tests, and it enables no privileged operation.
Normative contract: O5-DESIGN.r4 with addenda A1 and R5-1; see CONTRACT.md."""
