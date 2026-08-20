package workerboot

import (
	"go/ast"
	"go/printer"
	"go/token"
	"io"
)

// printNode renders an AST node back to Go source. Kept next to the S2
// entrypoint guard so its failure messages can quote the actual expression that
// broke the contract rather than a node type.
func printNode(w io.Writer, n ast.Node) error {
	return printer.Fprint(w, token.NewFileSet(), n)
}
