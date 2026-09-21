package bff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeBFF writes content into dir/rel, creating parent directories.
func writeBFF(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// entityLite and endpointLite are the projected assertion views of the scanner
// rows used by the tests.
type entityLite struct {
	Entity    string
	TableName string
	Col       string
	Type      string
	File      string
	Line      int
}

type endpointLite struct {
	Method string
	Path   string
	Resp   string
	Req    string
	File   string
	Line   int
}

// pkgMap parses a package.json document into the map form LooksLikeBackend
// expects.
func pkgMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// findEntity returns the first entity matching Entity+ColumnName.
func findEntity(t *testing.T, ents []*entityLite, entity, col string) *entityLite {
	t.Helper()
	for _, e := range ents {
		if e.Entity == entity && e.Col == col {
			return e
		}
	}
	return nil
}

func TestScanBFFGraphQL(t *testing.T) {
	dir := t.TempDir()
	writeBFF(t, dir, "schema.graphql", `type User {
  id: ID!
  name: String
  email: String
}

input NewUserInput {
  name: String!
}

enum Role {
  ADMIN
  USER
}

type Query {
  user(id: ID!): User
}

type Mutation {
  createUser(input: NewUserInput!): User
}
`)
	ents, eps, err := ScanBFF(dir)
	if err != nil {
		t.Fatal(err)
	}
	lit := make([]*entityLite, len(ents))
	for i, e := range ents {
		lit[i] = &entityLite{
			Entity: e.Entity, TableName: e.TableName, Col: e.ColumnName,
			Type: e.FieldType, File: e.SourceFile, Line: e.SourceLine,
		}
	}
	if len(ents) != 6 {
		t.Fatalf("entities = %d, want 6 (User 3 + NewUserInput 1 + Role 2): %+v", len(ents), ents)
	}
	if e := findEntity(t, lit, "User", "id"); e == nil || e.Type != "ID!" {
		t.Errorf("User/id entity = %+v, want Type ID!", e)
	} else if e.Line != 2 || e.File != "schema.graphql" {
		t.Errorf("User/id provenance = %s:%d, want schema.graphql:2", e.File, e.Line)
	}
	if e := findEntity(t, lit, "User", "email"); e == nil || e.Type != "String" {
		t.Errorf("User/email entity = %+v, want String", e)
	}
	if e := findEntity(t, lit, "NewUserInput", "name"); e == nil || e.Type != "String!" || e.TableName != "newuserinput" {
		t.Errorf("NewUserInput/name entity = %+v, want String! table newuserinput", e)
	}
	if e := findEntity(t, lit, "Role", "ADMIN"); e == nil || e.Type != "enum" {
		t.Errorf("Role/ADMIN entity = %+v, want enum", e)
	}

	got := map[string]*endpointLite{}
	for _, ep := range eps {
		got[ep.Method+" "+ep.Path] = &endpointLite{
			Method: ep.Method, Path: ep.Path, Resp: ep.ResponseType,
			Req: ep.RequestJSON, File: ep.SourceFile, Line: ep.SourceLine,
		}
	}
	if ep := got["QUERY user"]; ep == nil {
		t.Errorf("missing QUERY user, got %+v", got)
	} else if ep.Resp != "User" || ep.Req != "id: ID!" || ep.Line != 17 || ep.File != "schema.graphql" {
		t.Errorf("QUERY user = %+v, want Resp User Req 'id: ID!' line 17", ep)
	}
	if ep := got["MUTATION createUser"]; ep == nil {
		t.Errorf("missing MUTATION createUser, got %+v", got)
	} else if ep.Resp != "User" || ep.Req != "input: NewUserInput!" || ep.Line != 21 {
		t.Errorf("MUTATION createUser = %+v, want Req 'input: NewUserInput!' line 21", ep)
	}
	if len(eps) != 2 {
		t.Errorf("endpoints = %d, want 2: %+v", len(eps), eps)
	}
}

func TestScanBFFExpress(t *testing.T) {
	dir := t.TempDir()
	writeBFF(t, dir, "server.js", `const express = require('express');
const app = express();

app.get('/users/:id', (req, res) => {});
app.post('/users', (req, res) => {});
app.delete('/users/:id', (req, res) => {});
`)
	ents, eps, err := ScanBFF(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("entities = %d, want 0: %+v", len(ents), ents)
	}
	if len(eps) != 3 {
		t.Fatalf("endpoints = %d, want 3: %+v", len(eps), eps)
	}
	lines := map[string]int{}
	for _, ep := range eps {
		lines[ep.Method+" "+ep.Path] = ep.SourceLine
		if ep.SourceFile != "server.js" {
			t.Errorf("endpoint %s SourceFile = %q, want server.js", ep.Path, ep.SourceFile)
		}
		if ep.Method == "" || ep.Path == "" {
			t.Errorf("endpoint %+v has empty method/path", ep)
		}
	}
	if lines["GET /users/:id"] != 4 {
		t.Errorf("GET /users/:id line = %d, want 4", lines["GET /users/:id"])
	}
	if lines["POST /users"] != 5 {
		t.Errorf("POST /users line = %d, want 5", lines["POST /users"])
	}
	if lines["DELETE /users/:id"] != 6 {
		t.Errorf("DELETE /users/:id line = %d, want 6", lines["DELETE /users/:id"])
	}
}

func TestScanBFFNest(t *testing.T) {
	dir := t.TempDir()
	writeBFF(t, dir, "src/users.controller.ts", `import { Controller, Get, Post, Delete, Param } from '@nestjs/common';

@Controller('users')
export class UsersController {
  @Get(':id')
  async findOne(@Param('id') id: string) {
    return null;
  }

  @Post()
  create() {
    return null;
  }

  @Delete(':id')
  private remove(@Param('id') id: string) {}
}
`)
	ents, eps, err := ScanBFF(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("entities = %d, want 0: %+v", len(ents), ents)
	}
	got := map[string]*endpointLite{}
	for _, ep := range eps {
		got[ep.Method+" "+ep.Path] = &endpointLite{Method: ep.Method, Path: ep.Path, File: ep.SourceFile, Line: ep.SourceLine}
	}
	if ep := got["GET /users/:id"]; ep == nil {
		t.Errorf("missing GET /users/:id, got %+v", got)
	} else if ep.Line != 5 {
		t.Errorf("GET /users/:id line = %d, want 5", ep.Line)
	}
	if ep := got["POST /users"]; ep == nil {
		t.Errorf("missing POST /users, got %+v", got)
	} else if ep.Line != 10 {
		t.Errorf("POST /users line = %d, want 10", ep.Line)
	}
	if ep, ok := got["DELETE /users/:id"]; ok {
		t.Errorf("private DELETE /users/:id should be skipped, got %+v", ep)
	}
}

func TestScanBFFGQLTemplate(t *testing.T) {
	dir := t.TempDir()
	writeBFF(t, dir, "src/schema.ts",
		"import { gql } from 'graphql-tag';\n"+
			"\n"+
			"export const typeDefs = gql`\n"+
			"  type Item {\n"+
			"    id: ID!\n"+
			"    title: String\n"+
			"  }\n"+
			"\n"+
			"  type Query {\n"+
			"    items: [Item!]!\n"+
			"  }\n"+
			"`;\n"+
			"export const localSchema = graphql`type Local { x: Int }`;\n")
	ents, eps, err := ScanBFF(dir)
	if err != nil {
		t.Fatal(err)
	}
	var itemField, localField bool
	for _, e := range ents {
		if e.Entity == "Item" && e.ColumnName == "id" && e.FieldType == "ID!" && e.SourceFile == "src/schema.ts" {
			itemField = true
		}
		if e.Entity == "Local" && e.ColumnName == "x" && e.FieldType == "Int" {
			localField = true
		}
	}
	if !itemField || !localField {
		t.Errorf("entity extraction from gql templates failed (itemField=%v localField=%v): %+v", itemField, localField, ents)
	}
	var itemsFound bool
	for _, ep := range eps {
		if ep.Method == "QUERY" && ep.Path == "items" && ep.SourceFile == "src/schema.ts" {
			itemsFound = true
		}
	}
	if !itemsFound {
		t.Errorf("missing QUERY items from template, got %+v", eps)
	}
}

func TestLooksLikeBackend(t *testing.T) {
	// express dependency → backend.
	d1 := t.TempDir()
	writeBFF(t, d1, "package.json", `{"dependencies":{"express":"4.18.0"}}`)
	if !LooksLikeBackend(d1, pkgMap(t, `{"dependencies":{"express":"4.18.0"}}`)) {
		t.Error("express dependency should be detected as backend")
	}
	// @nestjs/core in devDependencies → backend.
	d2 := t.TempDir()
	writeBFF(t, d2, "package.json", `{"devDependencies":{"@nestjs/core":"9.0.0"}}`)
	if !LooksLikeBackend(d2, pkgMap(t, `{"devDependencies":{"@nestjs/core":"9.0.0"}}`)) {
		t.Error("@nestjs/core devDependency should be detected as backend")
	}
	// pure Vue (vite/vue, no entry file, no schema) → frontend.
	d3 := t.TempDir()
	writeBFF(t, d3, "package.json", `{"dependencies":{"vue":"3.0.0"},"devDependencies":{"vite":"5.0.0"}}`)
	if LooksLikeBackend(d3, pkgMap(t, `{"dependencies":{"vue":"3.0.0"},"devDependencies":{"vite":"5.0.0"}}`)) {
		t.Error("pure Vue module should not be detected as backend")
	}
	// Node entry file alone → backend (even with nil manifest).
	d4 := t.TempDir()
	writeBFF(t, d4, "server.ts", "console.log('hi');")
	if !LooksLikeBackend(d4, nil) {
		t.Error("server.ts entry file should be detected as backend")
	}
	// GraphQL schema file → backend.
	d5 := t.TempDir()
	writeBFF(t, d5, "schema.graphql", `type Query { ping: String }`)
	if !LooksLikeBackend(d5, nil) {
		t.Error("graphql schema file should be detected as backend")
	}
}
