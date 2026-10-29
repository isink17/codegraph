//go:build cgo

package treesitter

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestSolidityAdapterDeclarationsReferencesAndIdentity(t *testing.T) {
	const source = `pragma solidity ^0.8.20;
import {IToken as Token} from "./IERC20.sol";
abstract contract Vault is Token {
    address private _owner;
    Token private token;
    event Deposited(address indexed who, uint256 amount);
    event Deposited(uint256 indexed id, uint256 amount);
    error NotOwner(address who);
    struct Position { uint256 amount; }
    enum Status { Open, Closed }
    type Shares is uint128;
    constructor(address owner_, Token token_) { _owner = owner_; token = token_; }
    modifier onlyOwner(address expected) { _; }
    function withdraw(uint256 amount) external onlyOwner(msg.sender) returns (uint256) {
        token.transfer(_owner, amount);
        return amount;
    }
    function withdraw(address recipient, uint256 amount) external returns (uint256) {
        return token.transfer(recipient, amount);
    }
}
interface IVault { function balance(address who) external view returns (uint256); }
interface IErrors { error NotOwner(bytes32 role); }
library Math { function twice(uint256 value) internal pure returns (uint256) { return value * 2; } }
`
	adapter := NewSolidity()
	parsed, err := adapter.Parse(context.Background(), "contracts/Vault.sol", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Language != "solidity" || len(parsed.Edges) != 0 {
		t.Fatalf("language/edges = %q/%v, want solidity/no edges", parsed.Language, parsed.Edges)
	}
	if len(parsed.Imports) != 0 {
		t.Fatalf("imports were sent into generic import resolution: %v", parsed.Imports)
	}
	wantSymbols := map[string]struct {
		kind  string
		count int
	}{
		"Vault": {"class", 1}, "Vault.Deposited": {"event", 2}, "Vault.NotOwner": {"error", 1},
		"Vault.Position": {"struct", 1}, "Vault.Status": {"enum", 1}, "Vault.Shares": {"type", 1},
		"Vault.constructor": {"constructor", 1}, "Vault.onlyOwner": {"modifier", 1},
		"Vault.withdraw": {"function", 2}, "IVault": {"interface", 1}, "IErrors": {"interface", 1},
		"IErrors.NotOwner": {"error", 1}, "Math": {"class", 1}, "Math.twice": {"function", 1},
	}
	gotSymbols := map[string][]graph.Symbol{}
	for _, symbol := range parsed.Symbols {
		gotSymbols[symbol.QualifiedName] = append(gotSymbols[symbol.QualifiedName], symbol)
	}
	for qualified, want := range wantSymbols {
		matches := gotSymbols[qualified]
		if len(matches) != want.count {
			t.Errorf("%s symbols = %d, want %d", qualified, len(matches), want.count)
			continue
		}
		for _, symbol := range matches {
			if symbol.Kind != want.kind {
				t.Errorf("%s kind = %q, want %q", qualified, symbol.Kind, want.kind)
			}
			if symbol.Range.StartLine < 1 || symbol.Range.EndLine < symbol.Range.StartLine || symbol.Range.StartCol < 1 {
				t.Errorf("%s has invalid source range: %+v", qualified, symbol.Range)
			}
			if symbol.StableKey == "" {
				t.Errorf("%s has empty stable key", qualified)
			}
		}
	}
	withdrawals := gotSymbols["Vault.withdraw"]
	if withdrawals[0].StableKey == withdrawals[1].StableKey || withdrawals[0].Signature == withdrawals[1].Signature {
		t.Fatalf("overloads share identity/signature: %+v", withdrawals)
	}
	for _, qualified := range []string{"Vault.Deposited"} {
		declarations := gotSymbols[qualified]
		if declarations[0].StableKey == declarations[1].StableKey || declarations[0].Signature == declarations[1].Signature {
			t.Fatalf("%s overloads share identity/signature: %+v", qualified, declarations)
		}
	}
	if got := strings.Join([]string{withdrawals[0].ContainerName, withdrawals[1].ContainerName}, ","); got != "Vault,Vault" {
		t.Fatalf("overload containers = %q", got)
	}
	if got := gotSymbols["Vault.Shares"][0].Signature; got != "is uint128" {
		t.Fatalf("user-defined value type signature = %q, want source-proven underlying type", got)
	}
	var imports, calls, modifierInvocations int
	for _, ref := range parsed.References {
		switch ref.Kind {
		case "import":
			imports++
			if ref.Name != "./IERC20.sol" || ref.QualifiedName != ref.Name || ref.Range.StartLine != 2 {
				t.Errorf("import reference = %+v", ref)
			}
		case "call":
			calls++
			if ref.Range.StartLine < 1 || ref.Range.StartCol < 1 {
				t.Errorf("call reference has invalid range: %+v", ref)
			}
		case "modifier_invocation":
			modifierInvocations++
		}
	}
	if imports != 1 || calls < 2 || modifierInvocations != 1 {
		t.Fatalf("references import/call/modifier counts = %d/%d/%d, want 1/at least 2/1", imports, calls, modifierInvocations)
	}
	again, err := adapter.Parse(context.Background(), "contracts/Vault.sol", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := solidityStableKeys(again.Symbols), solidityStableKeys(parsed.Symbols); !reflect.DeepEqual(got, want) {
		t.Fatalf("stable keys changed across repeated parses: %v != %v", got, want)
	}
}

func TestSolidityStableIdentityIncludesScopeAndSignature(t *testing.T) {
	adapter := NewSolidity()
	parse := func(source string) []string {
		t.Helper()
		parsed, err := adapter.Parse(context.Background(), "contracts/Identity.sol", []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		return solidityStableKeys(parsed.Symbols)
	}
	base := parse(`contract A { function f(uint x) public {} } contract B { function f(uint x) public {} }`)
	changed := parse(`contract A { function f(address x) public {} } contract B { function f(uint x) public {} }`)
	if reflect.DeepEqual(base, changed) {
		t.Fatalf("signature change preserved every stable key: %v", base)
	}
	if len(base) != 4 || len(changed) != 4 {
		t.Fatalf("nested same-name declarations were lost: base=%v changed=%v", base, changed)
	}
	duplicates := parse(`contract A { function f(uint x) public {} function f(uint y) public {} }`)
	for _, key := range duplicates {
		if strings.Contains(key, "function:A.f:") {
			t.Fatalf("ambiguous exact-signature declarations were emitted: %v", duplicates)
		}
	}
	constructorA := parse(`contract A { constructor(address owner) {} }`)
	constructorB := parse(`contract A { constructor(address owner, uint amount) {} }`)
	if reflect.DeepEqual(constructorA, constructorB) {
		t.Fatalf("constructor signature change retained identity: %v", constructorA)
	}
}

func TestSolidityImportPathsAcceptBothQuoteStyles(t *testing.T) {
	parsed, err := NewSolidity().Parse(context.Background(), "imports.sol", []byte("import './Token.sol';\nimport \"./Vault.sol\";\n"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, ref := range parsed.References {
		if ref.Kind == "import" {
			paths = append(paths, ref.Name)
			if ref.SymbolID != nil || ref.ContextSymbolID != nil {
				t.Errorf("import reference was bound: %+v", ref)
			}
		}
	}
	if !reflect.DeepEqual(paths, []string{"./Token.sol", "./Vault.sol"}) {
		t.Fatalf("import paths = %v, want both source paths unbound", paths)
	}
}

func TestSolidityStringLiteralEscapes(t *testing.T) {
	for literal, want := range map[string]string{
		`'./Token\x2esol'`:   `./Token.sol`,
		`"./Vault\u002esol"`: `./Vault.sol`,
	} {
		if got, ok := solidityStringLiteral(literal); !ok || got != want {
			t.Errorf("solidityStringLiteral(%q) = %q, %v; want %q, true", literal, got, ok, want)
		}
	}
	for _, literal := range []string{`'./Token\q.sol'`, `"./Vault\x2.sol"`, `'./Token\uD800.sol'`} {
		if got, ok := solidityStringLiteral(literal); ok {
			t.Errorf("solidityStringLiteral(%q) = %q, true; want undecodable", literal, got)
		}
	}
}

func TestSolidityCallsStayUnresolvedAndMalformedSourceRecovers(t *testing.T) {
	adapter := NewSolidity()
	parsed, err := adapter.Parse(context.Background(), "contracts/Caller.sol", []byte(`contract Caller {
    function run(uint x) external { target(x); this.target(x); }
    function target(uint) internal {}
    function broken(uint public { target(); }
    fallback() external payable {}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Edges) != 0 {
		t.Fatalf("calls produced edges without semantic evidence: %+v", parsed.Edges)
	}
	for _, symbol := range parsed.Symbols {
		if symbol.Name == "fallback" || symbol.Name == "receive" {
			t.Fatalf("unsupported fallback syntax was forced into a symbol kind: %+v", symbol)
		}
	}
	for _, ref := range parsed.References {
		if ref.Kind == "call" && ref.Name == "target" && ref.SymbolID != nil {
			t.Fatalf("call reference was bound by spelling: %+v", ref)
		}
	}
}

func solidityStableKeys(symbols []graph.Symbol) []string {
	keys := make([]string, len(symbols))
	for i, symbol := range symbols {
		keys[i] = symbol.StableKey
	}
	return keys
}
