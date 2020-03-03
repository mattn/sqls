package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lighttiger2505/sqls/database"
	"github.com/olekukonko/tablewriter"
	"github.com/sourcegraph/jsonrpc2"
)

const (
	TDSKNone        TextDocumentSyncKind = 0
	TDSKFull        TextDocumentSyncKind = 1
	TDSKIncremental TextDocumentSyncKind = 2
)

type Server struct {
	db        database.Database
	files     map[string]*File
	completer *Completer
}

type File struct {
	LanguageID string
	Text       string
}

func NewServer() *Server {
	return &Server{
		files: make(map[string]*File),
	}
}

func (s *Server) init() error {
	s.completer = NewCompleter(s.db)
	if err := s.completer.Init(); err != nil {
		return err
	}
	return nil
}

func (s *Server) handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(ctx, conn, req)
	case "initialized":
		return
	case "shutdown":
		return s.handleShutdown(ctx, conn, req)
	case "textDocument/didOpen":
		return s.handleTextDocumentDidOpen(ctx, conn, req)
	case "textDocument/didChange":
		return s.handleTextDocumentDidChange(ctx, conn, req)
	case "textDocument/didSave":
		return s.handleTextDocumentDidSave(ctx, conn, req)
	case "textDocument/didClose":
		return s.handleTextDocumentDidClose(ctx, conn, req)
	case "textDocument/completion":
		return s.handleTextDocumentCompletion(ctx, conn, req)
		// case "textDocument/formatting":
		// 	return h.handleTextDocumentFormatting(ctx, conn, req)
		// case "textDocument/documentSymbol":
		// 	return h.handleTextDocumentSymbol(ctx, conn, req)
	case "textDocument/codeAction":
		return s.handleTextDocumentCodeAction(ctx, conn, req)
	case "workspace/executeCommand":
		return s.handleWorkspaceExecuteCommand(ctx, conn, req)
	case "workspace/didChangeConfiguration":
		return s.handleWorkspaceDidChangeConfiguration(ctx, conn, req)
	}

	return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: fmt.Sprintf("method not supported: %s", req.Method)}
}

func (s *Server) handleInitialize(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params InitializeParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	return InitializeResult{
		Capabilities: ServerCapabilities{
			TextDocumentSync:   TDSKFull,
			HoverProvider:      false,
			CodeActionProvider: true,
			CompletionProvider: &CompletionOptions{
				TriggerCharacters: []string{"."},
			},
			DefinitionProvider:              false,
			DocumentFormattingProvider:      false,
			DocumentRangeFormattingProvider: false,
		},
	}, nil
}

func (s *Server) handleShutdown(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	if s.db != nil {
		s.db.Close()
	}
	return nil, nil
}

func (s *Server) handleTextDocumentDidOpen(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params DidOpenTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if err := s.openFile(params.TextDocument.URI, params.TextDocument.LanguageID); err != nil {
		return nil, err
	}
	if err := s.updateFile(params.TextDocument.URI, params.TextDocument.Text); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Server) handleTextDocumentDidChange(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params DidChangeTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if err := s.updateFile(params.TextDocument.URI, params.ContentChanges[0].Text); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Server) handleTextDocumentDidSave(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params DidSaveTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if params.Text != "" {
		err = s.updateFile(params.TextDocument.URI, params.Text)
	} else {
		err = s.saveFile(params.TextDocument.URI)
	}
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Server) handleTextDocumentDidClose(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params DidCloseTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if err := s.closeFile(params.TextDocument.URI); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Server) handleTextDocumentCompletion(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params CompletionParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	f, ok := s.files[params.TextDocument.URI]
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}

	completionItems, err := s.completer.complete(f.Text, params)
	if err != nil {
		return nil, err
	}
	return completionItems, nil
}

func (s *Server) openFile(uri string, languageID string) error {
	f := &File{
		Text:       "",
		LanguageID: languageID,
	}
	s.files[uri] = f
	return nil
}

func (s *Server) closeFile(uri string) error {
	delete(s.files, uri)
	return nil
}

func (s *Server) updateFile(uri string, text string) error {
	f, ok := s.files[uri]
	if !ok {
		return fmt.Errorf("document not found: %v", uri)
	}
	f.Text = text
	return nil
}

func (s *Server) saveFile(uri string) error {
	return nil
}

func (h *Server) handleTextDocumentCodeAction(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params CodeActionParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	commands := []Command{
		{
			Title:     "Execute Query",
			Command:   "executeQuery",
			Arguments: []interface{}{params.TextDocument.URI},
		},
	}
	return commands, nil
}

func (s *Server) handleWorkspaceDidChangeConfiguration(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	var params DidChangeConfigurationParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if s.db != nil {
		s.db.Close()
	}
	s.db, err = database.Open(
		params.Settings.SQLS.Driver,
		params.Settings.SQLS.DataSourceName,
	)
	if err != nil {
		return nil, err
	}

	if err := s.init(); err != nil {
		return nil, fmt.Errorf("sqls: failed database connection: %v", err)
	}
	return nil, nil
}

func (s *Server) executeQuery(params ExecuteCommandParams) (result interface{}, err error) {
	if s.db == nil {
		return nil, errors.New("connection is closed")
	}
	if len(params.Arguments) != 1 {
		return nil, fmt.Errorf("invalid arguments for %s", params.Command)
	}
	uri, ok := params.Arguments[0].(string)
	if !ok {
		return nil, fmt.Errorf("invalid arguments for %s", params.Command)
	}
	f, ok := s.files[uri]
	if !ok {
		return nil, fmt.Errorf("document not found: %s", uri)
	}
	v, err := s.db.ExecuteQuery(context.Background(), f.Text)
	if err != nil {
		return nil, err
	}
	rows, ok := v.(sql.Rows)
	if !ok {
		return nil, nil
	}
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	table := tablewriter.NewWriter(&buf)
	table.SetHeader(columns)

	values := make([]sql.RawBytes, len(columns))
	scanArgs := make([]interface{}, len(values))
	for i := range values {
		scanArgs[i] = &values[i]
	}
	for rows.Next() {
		err = rows.Scan(scanArgs...)
		if err != nil {
			return nil, err
		}
		var cols []string
		for _, col := range values {
			if col == nil {
				cols = append(cols, "NULL")
			} else {
				cols = append(cols, string(col))
			}
		}
		table.Append(cols)
	}

	table.Render()
	return buf.String(), nil
}

func (s *Server) handleWorkspaceExecuteCommand(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params ExecuteCommandParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	switch params.Command {
	case "executeQuery":
		return s.executeQuery(params)
	}
	return nil, fmt.Errorf("unsupported command: %v", params.Command)
}
