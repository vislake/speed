/**
 * Namespace registration: the counterpart of go/pkgcore/i18n's
 * Builder.AddModule. One namespace per package, registered once per
 * instance, validated completely before anything is added.
 *
 * Validation is the discipline, mirroring the Go catalog's rules:
 *
 *  - language tags must be canonical and belong to the instance's
 *    supported set -- an unknown-language resource would be dead weight,
 *    so it is refused;
 *  - every supported language must be present, because a namespace that
 *    speaks fewer languages than the host would silently force the
 *    missing-language render-as-key path on users of the others;
 *  - all provided languages must carry the same leaf key set (parity with
 *    a reference language, sorted for deterministic messages), so a key
 *    can never exist in one language and silently miss in another;
 *  - leaves must be strings; nesting uses plain records only.
 *
 * Registration is atomic: any validation failure throws before a single
 * resource lands, and a namespace cannot be registered twice on the same
 * instance (that catches double-init in tests and SSR).
 */

import type { i18n as I18nInstance } from 'i18next'
import { readSupportedLanguages } from './languages'

/** A translation bundle: nested string leaves under plain records. */
export interface ResourceBundle {
  readonly [segment: string]: string | ResourceBundle
}

const NAMESPACE_PATTERN = /^[A-Za-z][A-Za-z0-9_-]*$/

/** Namespaces already registered per instance (WeakMap: no leak, per-instance). */
const registeredNamespaces = new WeakMap<I18nInstance, Set<string>>()

function describeLeaf(value: unknown): string {
  if (value === null) {
    return 'null'
  }
  if (Array.isArray(value)) {
    return 'an array'
  }
  return `a ${typeof value}`
}

function collectLeafPaths(
  bundle: ResourceBundle,
  prefix: string,
  leaves: Set<string>,
  namespace: string,
): void {
  for (const [key, value] of Object.entries(bundle)) {
    const path = prefix === '' ? key : `${prefix}.${key}`
    if (typeof value === 'string') {
      leaves.add(path)
      continue
    }
    if (typeof value !== 'object' || value === null || Array.isArray(value)) {
      throw new Error(
        `[speed-i18n] namespace "${namespace}": translation values must be strings or ` +
          `nested objects; "${path}" is ${describeLeaf(value)}.`,
      )
    }
    collectLeafPaths(value as ResourceBundle, path, leaves, namespace)
  }
}

function listSample(paths: Set<string>): string {
  const sorted = [...paths].sort()
  if (sorted.length <= 5) {
    return sorted.map((path) => `"${path}"`).join(', ')
  }
  const shown = sorted.slice(0, 5).map((path) => `"${path}"`)
  return `${shown.join(', ')} and ${sorted.length - 5} more`
}

/**
 * Register a package's translations under a namespace on the instance.
 *
 * resources maps canonical language tags to bundles; every key must be a
 * language the instance supports, and every supported language must be
 * present with the same leaf key set (see the module docs). Throws --
 * before mutating anything -- when any of that fails, or when the
 * namespace is already registered on this instance.
 */
export function registerNamespace(
  instance: I18nInstance,
  namespace: string,
  resources: Readonly<Record<string, ResourceBundle>>,
): void {
  if (!NAMESPACE_PATTERN.test(namespace)) {
    throw new Error(
      `[speed-i18n] namespace "${namespace}" is not a valid namespace: names must ` +
        "start with a letter and contain only letters, digits, '_' and '-'.",
    )
  }
  const registered = registeredNamespaces.get(instance)
  if (registered !== undefined && registered.has(namespace)) {
    throw new Error(
      `[speed-i18n] namespace "${namespace}" is already registered on this instance. ` +
        'A namespace registers exactly once per instance; double registration usually ' +
        'means a module was initialized twice (tests, SSR).',
    )
  }
  const supported = readSupportedLanguages(instance)
  if (supported.length === 0) {
    throw new Error(
      '[speed-i18n] registerNamespace needs the instance to pin a supported-language ' +
        'set; create the instance with createI18n (bare i18next instances are refused).',
    )
  }
  const languageKeys = Object.keys(resources)
  if (languageKeys.length === 0) {
    throw new Error(
      `[speed-i18n] namespace "${namespace}": resources must provide at least one language.`,
    )
  }
  const unsupported = languageKeys.filter(
    (language) => !supported.includes(language),
  )
  if (unsupported.length > 0) {
    throw new Error(
      `[speed-i18n] namespace "${namespace}": resource language(s) ` +
        `[${unsupported.join(', ')}] are not among the instance's supported languages ` +
        `[${supported.join(', ')}]; use canonical tags (e.g. "zh-CN"), one bundle per ` +
        'supported language.',
    )
  }
  const missing = supported.filter((language) => !languageKeys.includes(language))
  if (missing.length > 0) {
    throw new Error(
      `[speed-i18n] namespace "${namespace}" must cover every supported language; ` +
        `missing bundle(s) for [${missing.join(', ')}]. Each language's bundle must ` +
        'ship in the same registration.',
    )
  }

  const leafSets = new Map<string, Set<string>>()
  for (const language of languageKeys) {
    const bundle = resources[language]!
    const leaves = new Set<string>()
    collectLeafPaths(bundle, '', leaves, namespace)
    if (leaves.size === 0) {
      throw new Error(
        `[speed-i18n] namespace "${namespace}": the "${language}" bundle ships no ` +
        'translation keys.',
      )
    }
    leafSets.set(language, leaves)
  }

  const referenceLanguage = languageKeys[0]!
  const reference = leafSets.get(referenceLanguage)!
  for (const language of languageKeys) {
    const leaves = leafSets.get(language)!
    const missingKeys = [...reference].filter((path) => !leaves.has(path))
    const extraKeys = [...leaves].filter((path) => !reference.has(path))
    if (missingKeys.length > 0 || extraKeys.length > 0) {
      const parts: string[] = []
      if (missingKeys.length > 0) {
        parts.push(
          `missing in "${language}" relative to "${referenceLanguage}": ` +
            listSample(new Set(missingKeys)),
        )
      }
      if (extraKeys.length > 0) {
        parts.push(
          `present in "${language}" but not in "${referenceLanguage}": ` +
            listSample(new Set(extraKeys)),
        )
      }
      throw new Error(
        `[speed-i18n] namespace "${namespace}": the language bundles must carry the ` +
          `same key set; ${parts.join('; ')}.`,
      )
    }
  }

  const instanceNamespaces = registered ?? new Set<string>()
  instanceNamespaces.add(namespace)
  registeredNamespaces.set(instance, instanceNamespaces)
  for (const language of languageKeys) {
    instance.addResourceBundle(language, namespace, resources[language]!, true, true)
  }
}
