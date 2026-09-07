/**
 * photo-simulation-panel.tsx -- the block-B surface: one case photo's
 * smile-simulation workbench, rendered beneath the photo on the case
 * detail page. A clinic user picks an option set from the documented
 * vocabulary (three smile styles, three tooth shades, an adjustable
 * strength -- the same words the backend validates against; option
 * values travel verbatim into the simulate operation), starts the async
 * generation, watches an honest in-progress status, and once the job
 * completes sees the before/after comparison: the generated image
 * fetched through the simulation-content operation and rendered beside
 * the original photo, both over the blob-URL pattern the case page
 * uses.
 *
 * The surface's reads and writes go through the generated smilesim
 * operations over tenant-namespaced query keys, the same discipline as
 * the rest of the case page. The in-progress state is never a fake
 * spinner: after the 202 answer the panel polls the job-status route
 * and renders the job's live status text (starting, queued, generating,
 * retrying) in a status live region, ending the region only when the
 * job reaches a terminal state. Every refusal on the surface resolves
 * through the smile-simulation reachable-error map
 * (smile-sim-errors.ts) to bilingual text, never a raw code.
 */

import { useEffect, useMemo, useRef, useState } from 'react'
import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Slider from '@mui/material/Slider'
import Typography from '@mui/material/Typography'
import {
  getSmilesimGetJobQueryKey,
  getSmilesimGetSimulationContentQueryKey,
  getSmilesimListPhotoSimulationsQueryKey,
  useSmilesimGetJob,
  useSmilesimGetSimulationContent,
  useSmilesimListPhotoSimulations,
  useSmilesimSimulate,
} from '@speed/api-sdk'
import type {
  CasesPhoto,
  SmilesimSimulation,
  SmilesimSimulationOptionsSmileStyle,
  SmilesimSimulationOptionsToothShade,
} from '@speed/api-sdk'
import { useQueryClient } from '@tanstack/react-query'
import { useCurrentTenant } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import {
  smileSimErrorCodeOf,
  smileSimErrorTextKey,
} from '../smile-sim-errors.js'
import { base64ToBytes } from '../base64.js'
import { useDateFormatter } from '../use-date-formatter.js'
import { CasePhoto } from './case-photo.js'
import { SimulationShareAction } from './simulation-share-action.js'

/** The documented smile-style vocabulary, in server order (the ladder
 * from the most conservative change to the fullest one). The displayed
 * labels and the transmitted values both come from these constants --
 * the panel never offers an option the backend would refuse. */
const SMILE_STYLES: readonly SmilesimSimulationOptionsSmileStyle[] = [
  'subtle',
  'natural',
  'bright',
]

/** The documented tooth-shade vocabulary, in server order. */
const TOOTH_SHADES: readonly SmilesimSimulationOptionsToothShade[] = [
  'natural',
  'white',
  'ultra-white',
]

/** Each option value to its bundle text key. The bundle key is
 * camelCase where the wire value is hyphenated (ultra-white), so the
 * two never meet through string interpolation -- one record per
 * dimension, value-indexed, is the single mapping both the pickers and
 * the attempt summaries read. */
const SMILE_STYLE_TEXT_KEY: Readonly<
  Record<SmilesimSimulationOptionsSmileStyle, string>
> = {
  subtle: 'cases.sim.style.subtle',
  natural: 'cases.sim.style.natural',
  bright: 'cases.sim.style.bright',
}

const TOOTH_SHADE_TEXT_KEY: Readonly<
  Record<SmilesimSimulationOptionsToothShade, string>
> = {
  natural: 'cases.sim.shade.natural',
  white: 'cases.sim.shade.white',
  'ultra-white': 'cases.sim.shade.ultraWhite',
}

/** The lowest strength the slider offers: the backend refuses values at
 * or below zero (a billed no-op) and above full strength, so the
 * control's range stays strictly inside (0, 1]. */
const MIN_STRENGTH = 0.05
const MAX_STRENGTH = 1

/** A job status word on the wire to its text key in the app bundle. */
const STATUS_TEXT_KEY: Readonly<Record<string, string>> = {
  pending: 'cases.sim.status.pending',
  running: 'cases.sim.status.running',
  retrying: 'cases.sim.status.retrying',
  succeeded: 'cases.sim.status.succeeded',
  dead_letter: 'cases.sim.status.deadLetter',
  cancelled: 'cases.sim.status.cancelled',
}

/**
 * The credit cost of one smile simulation, mirrored from the service
 * that charges it (internal/smilesim/service.go's CreditsPerSimulation,
 * the flat cost Simulate reserves for one generation). The block-D
 * surface shows this number next to a completed generation -- a
 * generation must say what it cost -- and the mirror is pinned to the
 * Go source by simulation-cost-lockstep.test.ts, so the displayed price
 * cannot drift from the reserved amount without a failing test.
 */
export const SIMULATION_CREDIT_COST = 10

/** One of the option dimensions, rendered as native radio rows: each
 * option is a visible radio input whose label text is its accessible
 * name -- the block-B gate addresses every offered option by that
 * name. */
function OptionGroup({
  legendKey,
  name,
  options,
  value,
  onChange,
}: {
  readonly legendKey: string
  readonly name: string
  /** Each option: its transmitted value and its text key. */
  readonly options: ReadonlyArray<{
    readonly value: string
    readonly textKey: string
  }>
  readonly value: string
  readonly onChange: (value: string) => void
}): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  return (
    <Box
      component="fieldset"
      sx={{ border: 'none', margin: 0, padding: 0, minWidth: 0 }}
    >
      <Typography
        component="legend"
        variant="body2"
        color="text.secondary"
        sx={{ marginBottom: 0.5 }}
      >
        {t(legendKey)}
      </Typography>
      <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
        {options.map((option) => (
          <Box
            key={option.value}
            component="label"
            sx={{
              display: 'flex',
              alignItems: 'center',
              gap: 0.5,
              cursor: 'pointer',
            }}
          >
            <input
              type="radio"
              name={name}
              value={option.value}
              checked={value === option.value}
              onChange={() => onChange(option.value)}
            />
            <Typography component="span" variant="body2">
              {t(option.textKey)}
            </Typography>
          </Box>
        ))}
      </Box>
    </Box>
  )
}

/** The generated image of one succeeded simulation, fetched through the
 * simulation-content operation and rendered from a blob URL this
 * component owns and revokes -- the same pattern CasePhoto established
 * for the original photo's bytes. A failed read renders its code's
 * bilingual text, never a broken image. */
function SimulationResultImage({
  photoObjectID,
  simulation,
}: {
  readonly photoObjectID: string
  readonly simulation: SmilesimSimulation
}): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const currentTenant = useCurrentTenant()
  const tenantId = currentTenant?.tenantId ?? null

  const contentKey = useMemo(
    () => [
      'tenant',
      tenantId,
      ...getSmilesimGetSimulationContentQueryKey(
        photoObjectID,
        simulation.job_id,
      ),
    ],
    [tenantId, photoObjectID, simulation.job_id],
  )
  const contentQuery = useSmilesimGetSimulationContent(
    photoObjectID,
    simulation.job_id,
    { query: { queryKey: contentKey, enabled: tenantId !== null } },
  )

  const [objectUrl, setObjectUrl] = useState<string | null>(null)
  useEffect(() => {
    const data = contentQuery.data
    if (data === undefined) {
      return
    }
    const bytes = base64ToBytes(data.content_base64)
    const url = URL.createObjectURL(
      new Blob([bytes], { type: data.media_type }),
    )
    setObjectUrl(url)
    return () => {
      URL.revokeObjectURL(url)
    }
  }, [contentQuery.data])

  if (contentQuery.isError) {
    const code = smileSimErrorCodeOf(contentQuery.error)
    return (
      <Typography variant="body2" color="text.secondary">
        {t(smileSimErrorTextKey(code))}
      </Typography>
    )
  }
  if (objectUrl === null) {
    return <Box sx={{ height: 180 }} />
  }
  return (
    <Box
      component="img"
      src={objectUrl}
      alt={t('cases.sim.resultAlt')}
      sx={{
        display: 'block',
        maxWidth: 320,
        maxHeight: 240,
        objectFit: 'contain',
        borderRadius: 1,
        border: '1px solid',
        borderColor: 'divider',
      }}
    />
  )
}

/** The options one simulation was generated with, summarized in one
 * sentence -- shared by the comparison caption and the attempt rows. */
function attemptSummary(
  t: (key: string, options?: Record<string, unknown>) => string,
  simulation: SmilesimSimulation,
): string {
  // Every option dimension is pointer-shaped on the wire but always
  // present in a real answer (the server echoes the EFFECTIVE set); the
  // ??-defaults keep a shape-mismatched answer from crashing the row.
  const style = simulation.options.smile_style ?? 'natural'
  const shade = simulation.options.tooth_shade ?? 'natural'
  const strength = simulation.options.strength ?? MAX_STRENGTH
  return t('cases.sim.attemptSummary', {
    style: t(SMILE_STYLE_TEXT_KEY[style]),
    shade: t(TOOTH_SHADE_TEXT_KEY[shade]),
    strength: t('cases.sim.strengthPercent', {
      value: Math.round(strength * 100),
    }),
  })
}

/** The attempt history of this photo: every simulation the enumeration
 * lists, newest first, each with its live status text, its option
 * summary and the time it was requested -- honest history above and
 * below the comparison. */
function AttemptHistory({
  simulations,
  photoObjectID,
}: {
  readonly simulations: readonly SmilesimSimulation[]
  readonly photoObjectID: string
}): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)
  const formatDate = useDateFormatter(i18n.language)
  if (simulations.length === 0) {
    return (
      <Typography variant="body2" color="text.secondary" sx={{ marginTop: 2 }}>
        {t('cases.sim.noAttempts')}
      </Typography>
    )
  }
  return (
    <Box sx={{ marginTop: 2 }}>
      <Typography variant="body2" color="text.secondary">
        {t('cases.sim.attemptsHeading')}
      </Typography>
      <Box component="ul" sx={{ margin: 0, paddingLeft: 2 }}>
        {simulations.map((simulation) => (
          <Box
            component="li"
            key={`${photoObjectID}-${simulation.job_id}`}
            sx={{ marginTop: 0.5 }}
          >
            <Typography variant="body2">
              {t(
                STATUS_TEXT_KEY[simulation.status] ??
                  'cases.sim.errors.unknown',
              )}
            </Typography>
            <Typography variant="body2">
              {attemptSummary(t, simulation)}
            </Typography>
            <Typography variant="caption" color="text.secondary">
              {t('cases.sim.attemptDate', {
                date: formatDate(simulation.created_at),
              })}
            </Typography>
          </Box>
        ))}
      </Box>
    </Box>
  )
}

/** One case photo's smile-simulation workbench (see the file header). */
export function PhotoSimulationPanel({
  caseId,
  photo,
  index,
}: {
  readonly caseId: string
  readonly photo: CasesPhoto
  /** The photo's 1-based position on the case, for the original's alt
   * text inside the comparison. */
  readonly index: number
}): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)
  // The price the block-D surface renders beside a completed
  // generation, formatted in the surface language like every other
  // number on this page.
  const formatCost = useMemo(() => {
    const formatter = new Intl.NumberFormat(i18n.language)
    return (value: number): string => formatter.format(value)
  }, [i18n.language])
  const queryClient = useQueryClient()
  const currentTenant = useCurrentTenant()
  const tenantId = currentTenant?.tenantId ?? null
  const photoObjectID = photo.object_id

  // The option state defaults to the service's documented defaults
  // (natural smile, natural shade, full strength), so a panel the user
  // never touches generates exactly what a no-options simulate call
  // would.
  const [style, setStyle] =
    useState<SmilesimSimulationOptionsSmileStyle>('natural')
  const [shade, setShade] =
    useState<SmilesimSimulationOptionsToothShade>('natural')
  const [strength, setStrength] = useState(MAX_STRENGTH)
  // The job this panel started and is polling. Between the 202 answer
  // and the first poll the in-flight region says "starting" -- honest,
  // because the job exists (it was accepted) but its live status has
  // not been observed yet.
  const [pollingJobID, setPollingJobID] = useState<string | null>(null)
  // Whether the automatic first-preview run was refused outright (the
  // simulate call failed, so no job exists and no spend happened). The
  // auto-run is one-shot per panel lifetime, so a refusal is its end:
  // the cost disclosure must withdraw rather than keep promising a
  // spend that is no longer going to happen.
  const [autoPreviewRefused, setAutoPreviewRefused] = useState(false)

  const listKey = useMemo(
    () => [
      'tenant',
      tenantId,
      ...getSmilesimListPhotoSimulationsQueryKey(photoObjectID),
    ],
    [tenantId, photoObjectID],
  )
  const simulationsQuery = useSmilesimListPhotoSimulations(photoObjectID, {
    query: { queryKey: listKey, enabled: tenantId !== null },
  })

  const jobKey = useMemo(
    () => [
      'tenant',
      tenantId,
      ...(pollingJobID === null
        ? []
        : getSmilesimGetJobQueryKey(pollingJobID)),
    ],
    [tenantId, pollingJobID],
  )
  const jobQuery = useSmilesimGetJob(pollingJobID ?? '', {
    query: {
      queryKey: jobKey,
      enabled: tenantId !== null && pollingJobID !== null,
      refetchInterval: 1500,
    },
  })

  const simulate = useSmilesimSimulate()
  const simulations = simulationsQuery.data?.simulations ?? []

  // Whether this panel has already started a simulation on the person's
  // behalf (the automatic default preview below) -- one auto-run per
  // photo per panel lifetime, whatever else happens afterwards.
  const autoPreviewStartedRef = useRef(false)

  // The automatic default preview: a photo that has NO simulation
  // attempt of any kind yet -- the state every freshly attached photo
  // opens in -- is given one automatic generation with the service's
  // documented default options the moment this panel can act. The case
  // page opens on a photo with nothing but the pickers and a blank
  // promise otherwise; the practice's first look at a new patient
  // photo is the comparison this auto-run produces, and the block-C
  // gate's own journey opens a case this way. Any existing attempt --
  // queued, running, succeeded or failed -- leaves the panel hands-off,
  // and a person clicking Simulate is never doubled up: the button is
  // disabled while the auto-run is in flight, and the run's own
  // progress is the same honest in-flight region every generation
  // reports. Deliberately one-shot rather than re-runnable: a person
  // who changes the options and clicks Simulate starts an ordinary
  // second generation, exactly as before.
  useEffect(() => {
    if (tenantId === null) {
      return
    }
    if (simulationsQuery.isLoading || simulationsQuery.isError) {
      return
    }
    if (autoPreviewStartedRef.current) {
      return
    }
    if (simulations.length > 0) {
      return
    }
    if (pollingJobID !== null || simulate.isPending) {
      return
    }
    autoPreviewStartedRef.current = true
    simulate.mutate(
      {
        data: {
          photo_object_id: photoObjectID,
          options: {
            smile_style: style,
            tooth_shade: shade,
            strength,
          },
        },
      },
      {
        onSuccess: (jobRef) => {
          setPollingJobID(jobRef.job_id)
        },
        onError: () => {
          setAutoPreviewRefused(true)
        },
      },
    )
  }, [
    tenantId,
    photoObjectID,
    simulationsQuery.isLoading,
    simulationsQuery.isError,
    simulations.length,
    pollingJobID,
    simulate,
    simulate.isPending,
    style,
    shade,
    strength,
  ])

  const status = jobQuery.data?.status
  const terminal =
    status === 'succeeded' ||
    status === 'dead_letter' ||
    status === 'cancelled'

  // Settle the poll when the job reaches a terminal state: the live
  // region closes, and the photo's enumeration is refreshed once so its
  // rows (and the comparison, for a succeeded job) reflect the outcome.
  useEffect(() => {
    if (!terminal || pollingJobID === null) {
      return
    }
    setPollingJobID(null)
    void queryClient.invalidateQueries({ queryKey: listKey })
  }, [terminal, pollingJobID, queryClient, listKey])

  // A failed poll (the job is gone, say) settles the same way: the live
  // region cannot be honest about a job the route no longer finds.
  const pollFailed = jobQuery.isError
  useEffect(() => {
    if (!pollFailed || pollingJobID === null) {
      return
    }
    setPollingJobID(null)
    void queryClient.invalidateQueries({ queryKey: listKey })
  }, [pollFailed, pollingJobID, queryClient, listKey])

  // The in-flight region's honest state word: "starting" between the
  // 202 and the first poll answer, then the job's own live status for
  // as long as it stays non-terminal.
  const inflight =
    pollingJobID === null || pollFailed
      ? null
      : status === undefined
        ? 'starting'
        : terminal
          ? null
          : status

  const succeeded = simulations.filter(
    (entry) => entry.status === 'succeeded' && entry.output_object_id,
  )
  // The enumeration is newest first, so the first succeeded entry is the
  // newest result the comparison should show.
  const newestSucceeded = succeeded[0]

  const startSimulation = (): void => {
    simulate.mutate(
      {
        data: {
          photo_object_id: photoObjectID,
          options: {
            smile_style: style,
            tooth_shade: shade,
            strength,
          },
        },
      },
      {
        onSuccess: (jobRef) => {
          setPollingJobID(jobRef.job_id)
        },
      },
    )
  }
  const inFlightOrSubmitting =
    pollingJobID !== null || simulate.isPending

  const simulateErrorCode = simulate.isError
    ? smileSimErrorCodeOf(simulate.error)
    : null

  // Whether the automatic first-preview disclosure stands (see the
  // auto-run effect above: a photo with no simulation attempt of any
  // kind is given one automatic generation the moment this panel can
  // act, and that generation spends credits). The line must be on the
  // surface BEFORE the automatic run fires and stay while it is in
  // flight -- a spend with no cost connected to it is a silent one --
  // and it must never promise a spend the run will not make: a photo
  // the enumeration shows with attempts of its own is never auto-run
  // again (the run is one-shot per photo), so the line withdraws the
  // moment the answer shows any, and it withdraws on the auto-run's
  // own refusal, when no job exists and no spend happened. While the
  // enumeration is still loading the panel cannot yet know the
  // photo's history; the line then reads as the panel's standing rule
  // and is withdrawn immediately if the answer turns out to show
  // attempts.
  const listSettled = simulationsQuery.data !== undefined
  const showAutoPreviewCostNotice =
    tenantId !== null &&
    !autoPreviewRefused &&
    !simulationsQuery.isError &&
    (simulations.length === 0 || !listSettled)

  return (
    <Box sx={{ marginTop: 2, maxWidth: 720 }}>
      <Typography variant="h6">{t('cases.sim.title')}</Typography>

      {/* The cost disclosure of the automatic first preview (the
      product-disclosure gate): the panel is about to generate this
      photo's first preview by itself, and the surface says so and
      prices it before the run fires -- the person opening the case
      learns what the automatic preview spends before it spends it,
      not only from the after-the-fact cost line under a completed
      comparison. */}
      {showAutoPreviewCostNotice && (
        <Typography
          variant="body2"
          color="text.secondary"
          sx={{ marginTop: 0.5 }}
        >
          {t('cases.sim.autoPreviewCost', {
            value: formatCost(SIMULATION_CREDIT_COST),
          })}
        </Typography>
      )}

      <Box
        sx={{
          display: 'flex',
          flexDirection: 'column',
          gap: 1.5,
          marginTop: 1,
        }}
      >
        <OptionGroup
          legendKey="cases.sim.styleGroup"
          name={`smile-style-${photoObjectID}`}
          value={style}
          onChange={(next) =>
            setStyle(next as SmilesimSimulationOptionsSmileStyle)
          }
          options={SMILE_STYLES.map((option) => ({
            value: option,
            textKey: SMILE_STYLE_TEXT_KEY[option],
          }))}
        />
        <OptionGroup
          legendKey="cases.sim.shadeGroup"
          name={`tooth-shade-${photoObjectID}`}
          value={shade}
          onChange={(next) =>
            setShade(next as SmilesimSimulationOptionsToothShade)
          }
          options={TOOTH_SHADES.map((option) => ({
            value: option,
            textKey: TOOTH_SHADE_TEXT_KEY[option],
          }))}
        />
        <Box>
          <Box sx={{ display: 'flex', justifyContent: 'space-between' }}>
            <Typography
              variant="body2"
              color="text.secondary"
              id={`simulation-strength-${photoObjectID}`}
            >
              {t('cases.sim.strengthLabel')}
            </Typography>
            <Typography variant="body2" color="text.secondary">
              {t('cases.sim.strengthPercent', {
                value: Math.round(strength * 100),
              })}
            </Typography>
          </Box>
          <Slider
            aria-labelledby={`simulation-strength-${photoObjectID}`}
            value={strength}
            min={MIN_STRENGTH}
            max={MAX_STRENGTH}
            step={0.05}
            onChange={(_event, next) => setStrength(next as number)}
            sx={{ maxWidth: 360 }}
          />
        </Box>
      </Box>

      <Box
        sx={{ display: 'flex', alignItems: 'center', gap: 2, marginTop: 1 }}
      >
        <Button
          variant="contained"
          onClick={startSimulation}
          disabled={inFlightOrSubmitting || tenantId === null}
        >
          {t('cases.sim.simulate')}
        </Button>
        {simulateErrorCode !== null && (
          <Typography variant="body2" role="alert" color="error">
            {t(smileSimErrorTextKey(simulateErrorCode))}
          </Typography>
        )}
      </Box>

      {inflight !== null && (
        <Typography
          variant="body2"
          role="status"
          sx={{ display: 'block', marginTop: 1.5 }}
        >
          {t(STATUS_TEXT_KEY[inflight] ?? 'cases.sim.status.starting')}
        </Typography>
      )}

      {newestSucceeded !== undefined && (
        <Box
          role="region"
          aria-label={t('cases.sim.compareLabel')}
          sx={{ marginTop: 2 }}
        >
          <Box
            sx={{
              display: 'flex',
              gap: 2,
              flexWrap: 'wrap',
              alignItems: 'flex-start',
            }}
          >
            <CasePhoto caseId={caseId} photo={photo} index={index} />
            <SimulationResultImage
              photoObjectID={photoObjectID}
              simulation={newestSucceeded}
            />
          </Box>
          <Typography variant="caption" color="text.secondary">
            {attemptSummary(t, newestSucceeded)}
          </Typography>
          {/* The block-C share action: the practice turns this completed
          simulation into a patient-facing link for the before/after
          pair -- the photo the simulation was generated from and its
          output object. Mounted under the comparison with a key of the
          simulation's own job, so a newer result replaces a minted
          link by remount rather than carrying it over. */}
          <SimulationShareAction
            key={`share-${newestSucceeded.job_id}`}
            simulation={newestSucceeded}
            photoObjectId={photoObjectID}
          />
          {/* What this generation cost, where the generation happened
              (the block-D acceptance): the charge reads from the same
              mirror the service's reservation pins. */}
          <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
            {t('cases.sim.cost', {
              count: SIMULATION_CREDIT_COST,
              value: formatCost(SIMULATION_CREDIT_COST),
            })}
          </Typography>
        </Box>
      )}

      <AttemptHistory
        simulations={simulations}
        photoObjectID={photoObjectID}
      />
    </Box>
  )
}
