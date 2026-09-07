/**
 * The entry's export coverage: a mechanism, not a list.
 *
 * The package's public surface is exactly what src/index.ts re-exports,
 * and every public name a source module declares is public by definition
 * once that module is one of the entry's sources. Three public types
 * (EmptyStateHeadingLevel, DataTableColumnPriority, FormLayoutColumns)
 * each missed the entry in three successive feature commits, so the
 * recurrence is guarded structurally: this test walks the entry's own
 * export-from declarations, enumerates each resolved source module's
 * exported symbols with the TypeScript compiler API, and asserts the
 * entry re-exports every one of them. A fourth recurrence -- any public
 * name declared in a module the entry already exposes and forgotten in
 * the entry -- fails here instead of in a consumer's editor.
 *
 * The assertion is deliberately one-directional: the module list derives
 * from the entry's own declarations, so a whole new module the entry
 * does not expose at all (a component not yet wired in) is invisible to
 * this test by design -- the entry is the deliberate gate for that.
 */

import { readFileSync } from 'node:fs'
import { dirname, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import ts from 'typescript'

const srcDir = resolve(dirname(fileURLToPath(import.meta.url)))

/** The entry's relative from-specifiers (`export { ... } from './x.js'`). */
function exportFromSpecifiers(source: ts.SourceFile): string[] {
  const specifiers: string[] = []
  for (const statement of source.statements) {
    if (!ts.isExportDeclaration(statement)) {
      continue
    }
    const specifier = statement.moduleSpecifier
    if (specifier !== undefined && ts.isStringLiteral(specifier)) {
      specifiers.push(specifier.text)
    }
  }
  return specifiers
}

/** All exported symbol names of one source file (values and types alike). */
function exportedNames(program: ts.Program, source: ts.SourceFile): string[] {
  const checker = program.getTypeChecker()
  const moduleSymbol = checker.getSymbolAtLocation(source)
  if (moduleSymbol === undefined) {
    return []
  }
  return checker
    .getExportsOfModule(moduleSymbol)
    .map((symbol) => symbol.name)
    .filter((name): name is string => name !== undefined)
}

const options: ts.CompilerOptions = {
  module: ts.ModuleKind.NodeNext,
  moduleResolution: ts.ModuleResolutionKind.NodeNext,
  target: ts.ScriptTarget.ES2022,
  skipLibCheck: true,
}

describe('the entry export surface', () => {
  it('re-exports every public name its source modules declare', () => {
    const indexPath = resolve(srcDir, 'index.ts')
    const entrySource = ts.createSourceFile(
      indexPath,
      readFileSync(indexPath, 'utf8'),
      ts.ScriptTarget.ES2022,
      /* setParentNodes */ true,
      ts.ScriptKind.TS,
    )
    const moduleHost: ts.ModuleResolutionHost = {
      fileExists: ts.sys.fileExists,
      readFile: ts.sys.readFile,
    }
    const modulePaths = exportFromSpecifiers(entrySource).map((specifier) => {
      const resolved = ts.resolveModuleName(
        specifier,
        indexPath,
        options,
        moduleHost,
      )
      return resolved.resolvedModule?.resolvedFileName
    })
    expect(modulePaths.length).toBeGreaterThan(0)
    const program = ts.createProgram({
      rootNames: [indexPath, ...modulePaths.filter((p): p is string => p !== undefined)],
      options,
    })
    const entryFile = program.getSourceFile(indexPath)
    expect(entryFile).not.toBeUndefined()
    const entryNames = new Set(exportedNames(program, entryFile!))
    const missingAcrossModules: string[] = []
    for (const modulePath of modulePaths) {
      if (modulePath === undefined) {
        continue
      }
      const moduleFile = program.getSourceFile(modulePath)
      if (moduleFile === undefined) {
        continue
      }
      for (const name of exportedNames(program, moduleFile)) {
        if (!entryNames.has(name)) {
          missingAcrossModules.push(
            `${relative(srcDir, modulePath)}: ${name}`,
          )
        }
      }
    }
    expect(
      missingAcrossModules,
      'names a consumer cannot import from the package entry:',
    ).toEqual([])
  })
})
