"""Test-only OwnerCapability and evidence producers (LB5, LB3)."""

from .. import authz, provenance

SESSION = {"tty_dev": 16777220, "tty_ino": 1201, "session_leader_pid": 4242, "login_time": 1762819000}


def owner_capability(session=None, valid=lambda: True):
    return authz._new_capability(authz._SEAL, session or SESSION, valid)


def observed(name, sha256, role="decisive", prov=provenance.Provenance.ROOT_OBSERVED):
    """A ROOT_OBSERVED (or other decisive-provenance) component, as a PR-2/PR-3
    producer would construct it."""
    return provenance._produce(name, prov, role, sha256)
