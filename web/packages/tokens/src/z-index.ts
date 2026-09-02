/**
 * Z-index tokens.
 *
 * The slot names are exactly the MUI theme zIndex keys (mobileStepper, fab,
 * speedDial, appBar, drawer, modal, snackbar, tooltip) and the values match
 * MUI's defaults, so a ui-kit theme adapter assigns theme.zIndex unchanged.
 * The defaults encode a sane ordering for the whole platform; overlays that
 * must stack above a slot without renegotiating the scale can add to it.
 */

export type ZIndexKey =
  | 'mobileStepper'
  | 'fab'
  | 'speedDial'
  | 'appBar'
  | 'drawer'
  | 'modal'
  | 'snackbar'
  | 'tooltip'

export type ZIndexValues = { readonly [Key in ZIndexKey]: number }

/** The z-index section of the speed token tree (SpeedTokens["zIndex"]). */
export interface ZIndexTokens {
  readonly values: ZIndexValues
}

export const zIndexTokens: ZIndexTokens = {
  values: {
    mobileStepper: 1000,
    fab: 1050,
    speedDial: 1050,
    appBar: 1100,
    drawer: 1200,
    modal: 1300,
    snackbar: 1400,
    tooltip: 1500,
  },
}
