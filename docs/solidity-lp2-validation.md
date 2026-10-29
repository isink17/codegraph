# Solidity LP2 adapter validation

This document records the bounded CG-104 parser pilot evidence. The adapter is
syntax-only: it extracts source declarations and unbound references. It does
not resolve calls, imports, inheritance, overload selection, library dispatch,
modifier execution, or dynamic dispatch. No compiler-parity or EVM claims are
made.

## Grammar provenance

- Grammar: `JoranHonig/tree-sitter-solidity`
- Commit: `048fe686cb1fde267243739b8bdbec8fc3a55272`
- Generator: `tree-sitter-cli 0.25.8`
- Command: `tree-sitter generate --abi 14`
- Pinned Go runtime remains `github.com/smacker/go-tree-sitter v0.0.0-20240827094217-dd81d9e9be82`.
- Generated `parser.c` SHA-256: `77120ebcf942b8890e889edabe8f050caffd6113f8acb9a33195e31b9c32ae15`.
- Required `parser.h` SHA-256: `180b893c8734778fd32f372dfbc27bd6ad1cd2221f26150b31256ff6716320d2`.
- Upstream MIT license SHA-256: `8844f0cc9b76b9c8a9d0251904eb7536b6bb9976e0ec577e8f27ab96d42523ef`.

The parser, header and license hashes are checked in the CGO grammar package
unit test. The generator is needed only to regenerate or verify checked-in C;
ordinary builds need no Tree-sitter CLI. See
[`internal/parser/treesitter/soliditygrammar/PROVENANCE.md`](../internal/parser/treesitter/soliditygrammar/PROVENANCE.md)
for regeneration instructions and attribution.

## OpenZeppelin v5.2.0 corpus

Source repository `OpenZeppelin/openzeppelin-contracts`, commit
`acd4ff74de833399287ed6b31b4debf6b2b35527`. Selection: the first 30 `.sol`
paths below `contracts/` under bytewise (`LC_ALL=C`) lexicographic order.
SHA-256 is of each selected source file at that pinned commit.

| Path | SHA-256 |
|---|---|
| `contracts/access/AccessControl.sol` | `1086a1ad3788972b885ff3f209da510615dde6214d46b29e1cd2a4924f66c06d` |
| `contracts/access/IAccessControl.sol` | `c112f5c86fdc4e862f88d4f5694530342320923ebd5b5e7d9d8493550d337c2d` |
| `contracts/access/Ownable.sol` | `38578bd71c0a909840e67202db527cc6b4e6b437e0f39f0c909da32c1e30cb81` |
| `contracts/access/Ownable2Step.sol` | `c75345d7b9feef17ae29f2a800ca2f65f8021cf80436a41d6ec00f57f5180f36` |
| `contracts/access/extensions/AccessControlDefaultAdminRules.sol` | `fefdd334836d2fd96871284d4cc3877d95f081bbc54fc179a8572a5f2d50f7ad` |
| `contracts/access/extensions/AccessControlEnumerable.sol` | `d6c882c624699389abe9a4d8b6ad1f3d304ad6610221ac1cbd32f909e402629f` |
| `contracts/access/extensions/IAccessControlDefaultAdminRules.sol` | `c7bbf8bc9e68a131951d23f099b50e8d2737d07a0307dfcb26be0595f3efcfa6` |
| `contracts/access/extensions/IAccessControlEnumerable.sol` | `f91dc4b5750c1bac7a6a11a6057f14f01121847e6a6284e1200d8a87a4c4ff1e` |
| `contracts/access/manager/AccessManaged.sol` | `6834e350bc8bedbb042a62f6681f7976186e92e7febac2575bae081d81244ea9` |
| `contracts/access/manager/AccessManager.sol` | `72f06a0f03f2044d9b3bf036933aca7d589f0b8875a596206d50b696e59e8781` |
| `contracts/access/manager/AuthorityUtils.sol` | `6a78662d327e8c82eee93a4659ddfb62a1f477f8cd9a2319848b3566e40baac8` |
| `contracts/access/manager/IAccessManaged.sol` | `b6cbb31989224829b1bcbcb7c34088eb16362650174da2cc98b2d7e01a1609bd` |
| `contracts/access/manager/IAccessManager.sol` | `11da09673a4026c23d15f88562cc8cfff23646fb8264dad62db7f6ff1e992297` |
| `contracts/access/manager/IAuthority.sol` | `9fc27c63a17ed287dd1070ccca4bc9d3250af3d9e0c2c4ebf95173395ed98726` |
| `contracts/account/utils/draft-ERC4337Utils.sol` | `63f068487d71a4b6cd9f274cf21ec5a0eea264d24d52e1f22d3b595a6fac08b1` |
| `contracts/account/utils/draft-ERC7579Utils.sol` | `522d649a8edb4c6d94c1ee59661b00c146c64f20c7bf329512387bdeb41d4e37` |
| `contracts/finance/VestingWallet.sol` | `585d97b53f17da05cf97080a127a348ea68de5aa5228600c931c7f87f934948c` |
| `contracts/finance/VestingWalletCliff.sol` | `14981dea962b6c81e253295b702f2a461d7db60ae93715b293e95028d51b81f9` |
| `contracts/governance/Governor.sol` | `063125f0ce359e1bf169c2ff8989e0b8f5ffc87b766398c158d229be1e147dd4` |
| `contracts/governance/IGovernor.sol` | `2f73642a69d9ff9033ffbb835dfac260c523e32d65e0eec3259857e5934882bc` |
| `contracts/governance/TimelockController.sol` | `89a4cf0e404a42f13d01efefb5c1b3b1d46641c670b626b1b885330f3faccd2a` |
| `contracts/governance/extensions/GovernorCountingFractional.sol` | `06fd91844a6e6c6ed4425cb5238e7c6dc2a99499d6b266286ed9ebdc8e34c4ec` |
| `contracts/governance/extensions/GovernorCountingOverridable.sol` | `dc35cfdbcf97f991b7259dc37ff20176dfbed315fb7a4aeb47b8df34c3cdd47f` |
| `contracts/governance/extensions/GovernorCountingSimple.sol` | `fee9f14eae7b45504e19f2e5dd55e74d7ce05f9733157a5811bd883b80256639` |
| `contracts/governance/extensions/GovernorPreventLateQuorum.sol` | `aa4b34f37e8ef9b9a934afd4558ac87c7ebfd5074c4cbff5e107d42a62c8384a` |
| `contracts/governance/extensions/GovernorSettings.sol` | `f6268357cb5aeb8c2509699f78127b9b360e9bb57c2b4cb1406ca24a4573d66e` |
| `contracts/governance/extensions/GovernorStorage.sol` | `55ba9882c6d66026a6024cfe1db873754b0974a3859f709b765e35089e13d64c` |
| `contracts/governance/extensions/GovernorTimelockAccess.sol` | `676584f6c287cd0efbf388bdf0881a1654f63562b9a7ea6be03cc1a93c938266` |
| `contracts/governance/extensions/GovernorTimelockCompound.sol` | `ee11bd77cdfeaa113b804edb9ebc3c43e8fdc4a3f8c8dbd618af45e9f8a3b35a` |
| `contracts/governance/extensions/GovernorTimelockControl.sol` | `4fe02228a581a7b5f0f322834b79a898c5d65e1222573ca8afc0334feb6ae48b` |

Indexing these 30 files produced 556 symbols: 23 containers (`class`), 13
constructors, 4 enums, 55 custom errors, 47 events, 384 functions, 7
interfaces, 6 modifiers, 12 structs and 5 user-defined value types. It
recorded 684 unbound references: 562 calls, 87 imports and 35 modifier
invocations. No edges or generic file-import rows were emitted; reference
symbol and context targets remained null.

Representative source checks included `Ownable` (abstract contract),
`Ownable.onlyOwner`, `AccessControl.RoleData`, and
`AccessControl.grantRole(bytes32,address)`. Seven same-file overloaded
function names had distinct signature-derived keys, including
`VestingWallet.release()` and `VestingWallet.release(address)`. Fresh index,
no-op update and forced fresh rebuild produced the same normalized graph
projection digest:
`8a1ac053fbc6f4693fa967dfef8cf4ed4c0bf653099116be3c36422c66ed52d4`.
This is parser/indexer evidence, not a claim that all Solidity declarations or
semantics are modelled.

The pinned fixture subset was also compiled with `solc-js 0.8.20`; the compiler
reported no errors (SPDX warnings only). This limited check does not imply
compiler parity. The grammar's upstream corpus has 126 tests; see the test
results in the implementation review/CI record.
