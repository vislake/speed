/**
 * The reference-app web host's own message bundle. The app registers this
 * namespace alongside the five namespace-shipping packages it composes
 * (ui-kit, layout-kit, auth-ui, tenancy-ui, account-ui); B1 ships only the
 * skeleton keys the bootstrap placeholder view renders, later iterations
 * extend it as surfaces land.
 */

import type { ResourceBundle } from '@speed/i18n'

import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

export const REFERENCE_APP_NAMESPACE = 'reference-app' as const

export const referenceAppResources: Readonly<Record<string, ResourceBundle>> = {
  'zh-CN': zhCN as ResourceBundle,
  'en-US': enUS as ResourceBundle,
}
