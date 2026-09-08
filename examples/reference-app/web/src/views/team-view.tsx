/**
 * team-view.tsx -- the team surface of the reference-app web host: who
 * works in the current clinic and who has been invited but has not
 * joined, plus the invite-a-colleague flow the practice's second
 * employee is added through.
 *
 * The reads go through the app's own hand-written calls over the one
 * RequestFn the host bound, on a tenant-namespaced query key
 * (['tenant', tenantId, 'team']) so a tenant switch can never read the
 * previous clinic's roster -- user-menu.tsx evicts the departing
 * tenant's ['tenant', tenantId] queries and main.tsx empties the whole
 * cache the moment the session ends, exactly as the notes surface's own
 * key relies on. The members half of the roster reads the app's OWN
 * roster-with-identity answer (team-api.ts -- GET
 * /api/reference-app/team-members, the host composition
 * cmd/server/team_members.go mounts, which enriches org's membership
 * rows with each member's display identity from authn's users table,
 * because org's fragment ships a backend leg only AND its member rows
 * carry opaque user ids by its own module-boundary rule); the pending
 * invitations and the clinic's root node read org's own surface
 * (org-api.ts -- go/org's fragment ships a backend leg only, the same
 * status go/sharing's holds, so no generated operation exists for
 * either host).
 *
 * One snapshot query answers the whole surface -- members, pending
 * invitations and the clinic's root node -- so the surface has exactly
 * one loading state, one error state and one gate, the notes-view
 * shape: the read failure is classified error-first (an error state is
 * a failure whatever it carried; only the rbac gate's own 403 code,
 * rbac.permission_denied, is an authorization fact and maps to
 * 'denied'), and the gated content holds the members roster, the
 * pending-invitations list and the invite flow.
 *
 * The gate is real in both directions. A caller without org:read (the
 * demo's reader role holds notes:read and nothing else) is refused on
 * the read itself and sees the no-permission suit, never a control
 * that would fail on click; a caller WITH the read but without
 * org:invite_member sees the invite form and is refused on the send,
 * the mutation probing the write gate the way notes' create probes
 * notes:write.
 *
 * NAMING THE ROSTER
 *
 * The roster names each member from the identity the composition
 * answered -- the display name the account registered with when it
 * typed one, the account's email otherwise -- with one exception: the
 * signed-in caller's own membership row is "You", never their identity
 * back at them. A member whose account row the composition could not
 * read (a vanished account) answers empty identity fields and the row
 * renders the surface's fallback label rather than the raw user id -- a
 * raw id where a colleague's identity belongs is exactly what this
 * surface must never render, and nothing in this view renders a userId
 * as a name. The user id itself stays on the row as its identity key:
 * the "You" naming and the row's react-query key compare against it,
 * but it never reaches the member column.
 *
 * The invitation half of the surface is unchanged by all of this and
 * needs none of it: org holds the address it was asked to invite, so a
 * pending invitation's row shows the address while the inviting
 * owner's own session remembers typing it (the module-level memory
 * below, paired with the row the create answered by its invitation
 * id). An invitation a different browser or an earlier session sent
 * stays listed -- it is the server's row, still pending -- under the
 * "invited from another device" label rather than a fabricated
 * address.
 *
 * That last pairing is what keeps the roster honest in both
 * directions: the invitation is not "sent" because a banner said so
 * and forgotten by the list -- the row the server lists is the
 * authority, and the address this session remembers is attached to it
 * -- and when the invitee accepts, the row leaves the pending list and
 * the member appears in the roster, so an owner can tell an invited
 * colleague from one who has joined.
 */

import { useMemo, useState } from 'react'
import type { ReactElement } from 'react'
import Alert from '@mui/material/Alert'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import TextField from '@mui/material/TextField'
import Typography from '@mui/material/Typography'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useAuthState, useCurrentTenant } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import type { RouteGuardStatus } from '@speed/layout-kit'
import { RouteGuard } from '@speed/layout-kit'
import type { DataTableColumn } from '@speed/ui-kit'
import { DataTable, EmptyState, FormField, FormLayout } from '@speed/ui-kit'
import { useForm } from 'react-hook-form'
import { useAppServices } from '../app-services.js'
import type { OrgInvitation } from '../org-api.js'
import {
  createOrgInvitation,
  listOrgInvitations,
  listOrgNodes,
} from '../org-api.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { fetchTeamMembers } from '../team-api.js'
import type { TeamMember } from '../team-api.js'
import { CurrentClinicLine } from './current-clinic.js'

/** The read gate's own refusal code, like the notes surface's: the rbac
 * layer's 403, the only list-read answer that is an authorization fact. */
const TEAM_READ_DENIED_CODE = 'rbac.permission_denied'

/** A failure that carries no code (a bug-shaped throw, an un-normalized
 * answer) collapses to this -- not a code @speed/api-client ever emits,
 * so the submit resolver treats it as unknown. */
const UNKNOWN_FAILURE_CODE = 'client.unknown'

/** A membership status's bundle key by the server's vocabulary value.
 * Statuses outside the vocabulary (a future addition) render as their
 * raw value rather than an untranslated guess. */
const MEMBER_STATUS_TEXT_KEYS: Readonly<Record<string, string>> = {
  active: 'team.members.statusActive',
  invited: 'team.members.statusInvited',
  suspended: 'team.members.statusSuspended',
}

/**
 * The reachable codes of an invite send, each mapped to the
 * app-namespace key carrying its current-language text. The org codes
 * are go/org/errors.go's sentinels for the create operation (the
 * address refusal, the node gone stale under the form, the two
 * invitation gates, the internal envelope), rbac.permission_denied is
 * the write gate's answer for a caller with the read but not the
 * invite permission, and the client.* trio is the transport contract.
 * An answer that is not on this list (a future server code) resolves
 * to the unknown fallback, so the surface never shows a raw key or
 * another language's text. Exported for the surface's error whitelist
 * to be deep-imported by the codes-alignment suite, like every other
 * surface whitelist in this app.
 */
export const TEAM_ERROR_TEXT_KEYS: Readonly<Record<string, string>> = {
  'rbac.permission_denied': 'team.invite.errors.permissionDenied',
  'org.invalid_email': 'team.invite.errors.invalidEmail',
  'org.node_not_found': 'team.invite.errors.nodeNotFound',
  'org.invitation_rate_limited': 'team.invite.errors.rateLimited',
  'org.invitations_disabled': 'team.invite.errors.invitationsDisabled',
  'org.internal_error': 'team.invite.errors.internalError',
  'client.network': 'team.invite.errors.client',
  'client.timeout': 'team.invite.errors.client',
  'client.protocol': 'team.invite.errors.client',
}

/**
 * The session-scoped memory pairing an invitation the owner sent with
 * the address they typed. org's invitation rows never name the invitee
 * (see the file header), so this module-level memory is the only place
 * the roster can learn an address from -- kept for the page session
 * (the app persists nothing by design; a reload starts anonymous and
 * every memory with it). Rows the server lists that this memory cannot
 * name -- an invitation another device sent -- render the surface's
 * "invited from another device" label instead of a fabricated address.
 */
const invitedAddressByInvitationID = new Map<string, string>()

/** Records the address an invite answered by the invitation row's own
 * id, so the refetched pending list can name the row. */
function rememberInvitedAddress(invitationID: string, address: string): void {
  invitedAddressByInvitationID.set(invitationID, address)
}

/** The address this session invited under the given invitation id, or
 * null when this session did not send that invitation. */
function invitedAddressOf(invitationID: string): string | null {
  return invitedAddressByInvitationID.get(invitationID) ?? null
}

/** Empties the session memory -- test support only, so suites start
 * clean. */
export function clearInvitedAddressMemory(): void {
  invitedAddressByInvitationID.clear()
}

/** The one field of the invite form: the colleague's address. */
interface InviteDraft {
  readonly email: string
}

/**
 * The code of an ApiError-shaped failure, or null for a failure that
 * carries none (a bug-shaped throw, an un-normalized answer).
 */
function apiErrorCodeOf(error: unknown): string | null {
  if (typeof error !== 'object' || error === null) {
    return null
  }
  const code = (error as { code?: unknown }).code
  return typeof code === 'string' && code.length > 0 ? code : null
}

/** A submit failure's code, collapsed to the never-whitelisted unknown
 * marker when the failure carried none, so the resolver always has a
 * code to show. */
function submitErrorCodeOf(error: unknown): string {
  return apiErrorCodeOf(error) ?? UNKNOWN_FAILURE_CODE
}

/**
 * The team surface: heading and clinic line, then the gated content --
 * the members roster, the invite flow and the pending invitations.
 */
export function TeamView(): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)
  const queryClient = useQueryClient()
  const { api } = useAppServices()
  const currentTenant = useCurrentTenant()
  const tenantId = currentTenant?.tenantId ?? null
  // The signed-in caller's own user id, for the "You" naming of their
  // own membership row (see the file header's naming section). The
  // hook is read-only and fails closed to null while anonymous.
  const principalUserId = useAuthState().principal?.user_id ?? null

  const [inviteFormOpen, setInviteFormOpen] = useState(false)
  const [submitErrorCode, setSubmitErrorCode] = useState<string | null>(null)
  const [sentNotice, setSentNotice] = useState(false)

  // The one snapshot query behind the whole surface (see the file
  // header): members, pending invitations and the clinic's root node,
  // fetched together so the surface has a single loading/error/gate
  // state. The root node is the invitee's binding target -- the demo
  // clinics are single-root practices, and an invitation must name the
  // node the invitee will join.
  const teamKey = useMemo(
    () => ['tenant', tenantId, 'team'],
    [tenantId],
  )
  const teamQuery = useQuery({
    queryKey: teamKey,
    queryFn: async () => {
      const [membersAnswer, invitationsAnswer, nodesAnswer] = await Promise.all([
        fetchTeamMembers(api),
        listOrgInvitations(api),
        listOrgNodes(api),
      ])
      const nodes = nodesAnswer.nodes ?? []
      const root = nodes.find((node) => node.depth === 0) ?? null
      return {
        members: membersAnswer.members ?? [],
        invitations: invitationsAnswer.invitations ?? [],
        rootNode: root,
      }
    },
    enabled: tenantId !== null,
  })

  // The read failure, classified error-first exactly like the notes
  // surface (see notes-view.tsx's own account of why the error state
  // must be consulted before anything else): only the rbac gate's own
  // refusal is an authorization fact; every other failed read is a
  // load failure and renders the error suit, never the no-permission
  // one.
  const listReadFailed = teamQuery.isError
  const listErrorCode = listReadFailed ? apiErrorCodeOf(teamQuery.error) : null
  const gateDenied = listErrorCode === TEAM_READ_DENIED_CODE
  const gateStatus: RouteGuardStatus = gateDenied
    ? 'denied'
    : teamQuery.data !== undefined
      ? 'allowed'
      : 'pending'

  const inviteForm = useForm<InviteDraft>({ defaultValues: { email: '' } })

  /** Opens the invite form, clearing the previous send's notice and
   * refusal so neither lingers over a fresh attempt. */
  function openInviteForm(): void {
    setInviteFormOpen(true)
    setSentNotice(false)
    setSubmitErrorCode(null)
  }

  /** Sends the invitation: the server validates the address, records
   * the pending invitation and mails the invitee's link. On success
   * the row's address joins the session memory under the row's own id
   * and the pending list is refetched, so the served list -- not a
   * banner -- is what shows the invitation standing. A refused send
   * keeps the form and shows the answer's code text. */
  async function handleSend(values: InviteDraft): Promise<void> {
    const rootNode = teamQuery.data?.rootNode
    if (rootNode === null || rootNode === undefined) {
      return
    }
    setSubmitErrorCode(null)
    setSentNotice(false)
    let invitation: OrgInvitation
    try {
      invitation = await createOrgInvitation(api, {
        email: values.email,
        nodeId: rootNode.id,
      })
    } catch (error) {
      setSubmitErrorCode(submitErrorCodeOf(error))
      return
    }
    rememberInvitedAddress(invitation.id, values.email)
    inviteForm.reset({ email: '' })
    setInviteFormOpen(false)
    setSentNotice(true)
    await queryClient.invalidateQueries({ queryKey: teamKey })
  }

  /** Resolves a submit-failure code to its current-language text. */
  function submitErrorText(code: string): string {
    const key = TEAM_ERROR_TEXT_KEYS[code] ?? 'team.invite.errors.unknown'
    return t(key)
  }

  // Cells render dates through Intl in the surface language; an
  // unparseable value renders as an empty cell rather than throwing.
  const formatDate = useMemo(() => {
    const formatter = new Intl.DateTimeFormat(i18n.language, {
      dateStyle: 'medium',
      timeStyle: 'short',
    })
    return (value: string | undefined): string => {
      if (value === undefined) {
        return ''
      }
      const date = new Date(value)
      return Number.isNaN(date.getTime()) ? '' : formatter.format(date)
    }
  }, [i18n.language])

  const memberColumns: readonly DataTableColumn<TeamMember>[] = useMemo(
    () => [
      {
        id: 'member',
        header: t('team.members.memberColumn'),
        cell: (membership) => {
          // The naming ladder of one roster row: the caller's own row is
          // "You" (compared by the row's user id, the identity key the
          // composition kept), a member who registered a display name is
          // named by it, a member who registered none is named by the
          // email their account was registered with, and a member whose
          // account row answered no identity at all renders the
          // fallback label. The raw user id is never rendered as a
          // name.
          if (membership.userId === principalUserId) {
            return t('team.members.youRow')
          }
          if (membership.displayName !== '') {
            return membership.displayName
          }
          if (membership.email !== '') {
            return membership.email
          }
          return t('team.members.identityUnknown')
        },
      },
      {
        id: 'status',
        header: t('team.members.statusColumn'),
        cell: (membership) => {
          // The known membership vocabulary renders through the bundle;
          // an unknown status renders as the raw value -- an opaque
          // server reference, deliberately untranslated (the account
          // surface's treatment of unknown tokens).
          const key = MEMBER_STATUS_TEXT_KEYS[membership.status]
          return key === undefined ? membership.status : t(key)
        },
      },
      {
        id: 'joined',
        header: t('team.members.joinedColumn'),
        cell: (membership) => formatDate(membership.createdAt),
      },
    ],
    [formatDate, principalUserId, t],
  )

  const invitationColumns: readonly DataTableColumn<OrgInvitation>[] =
    useMemo(
      () => [
        {
          id: 'address',
          header: t('team.invitations.addressColumn'),
          cell: (invitation) =>
            invitedAddressOf(invitation.id) ??
            t('team.invitations.unknownRecipient'),
        },
        {
          id: 'status',
          header: t('team.invitations.statusColumn'),
          cell: (invitation) =>
            invitation.status === 'pending'
              ? t('team.status.pending')
              : invitation.status,
        },
        {
          id: 'sent',
          header: t('team.invitations.sentColumn'),
          cell: (invitation) => formatDate(invitation.createdAt),
        },
        {
          id: 'expires',
          header: t('team.invitations.expiresColumn'),
          cell: (invitation) => formatDate(invitation.expiresAt),
        },
      ],
      [formatDate, t],
    )

  return (
    <Box sx={{ p: { xs: 2, sm: 3 }, maxWidth: 720 }}>
      <Typography component="h1" variant="h4" sx={{ fontWeight: 600 }}>
        {t('team.heading')}
      </Typography>
      <CurrentClinicLine />
      <Typography
        variant="body1"
        color="text.secondary"
        sx={{ marginTop: 1, marginBottom: 3 }}
      >
        {t('team.intro')}
      </Typography>
      {listReadFailed && !gateDenied ? (
        // The read failed for a reason other than authorization -- the
        // error empty state in its own suit, never the no-permission
        // one (a down server is not a permission problem).
        <EmptyState variant="error" headingLevel="h2" />
      ) : (
        <RouteGuard
          status={gateStatus}
          deniedFallback={
            <EmptyState variant="noPermission" headingLevel="h2" />
          }
        >
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            <section>
              <Typography component="h2" variant="h6" sx={{ marginBottom: 1 }}>
                {t('team.members.heading')}
              </Typography>
              <DataTable
                rows={teamQuery.data?.members ?? []}
                columns={memberColumns}
                rowKey={(membership) => membership.membershipId}
                loading={teamQuery.isFetching}
                emptyTitle={t('team.members.emptyTitle')}
                emptyDescription={t('team.members.emptyDescription')}
              />
            </section>
            <section>
              <Typography component="h2" variant="h6" sx={{ marginBottom: 1 }}>
                {t('team.invitations.heading')}
              </Typography>
              {sentNotice && (
                <Alert severity="success" role="status" sx={{ marginBottom: 2 }}>
                  {t('team.invite.sentNotice')}
                </Alert>
              )}
              {/* The invite affordance renders only while this clinic has
                  a root node to bind an invitee to -- a clinic whose tree
                  is missing cannot receive members, so no control is drawn
                  that would fail on click. */}
              {teamQuery.data?.rootNode !== undefined && !inviteFormOpen ? (
                <Button
                  variant="contained"
                  onClick={openInviteForm}
                  sx={{ marginBottom: 2 }}
                >
                  {t('team.invite.action')}
                </Button>
              ) : null}
              {inviteFormOpen && teamQuery.data?.rootNode !== undefined && (
                <FormLayout
                  form={inviteForm}
                  onSubmit={handleSend}
                  actions={
                    <Button
                      type="submit"
                      variant="contained"
                      disabled={inviteForm.formState.isSubmitting}
                    >
                      {t('team.invite.send')}
                    </Button>
                  }
                >
                  <FormField
                    name="email"
                    required
                    rules={{
                      required: t('team.invite.emailRequired'),
                      pattern: {
                        value: /^[^\s@]+@[^\s@]+\.[^\s@]+$/,
                        message: t('team.invite.emailInvalid'),
                      },
                    }}
                    render={({ field, invalid, errorText }) => (
                      <TextField
                        {...field}
                        type="email"
                        label={t('team.invite.emailLabel')}
                        fullWidth
                        error={invalid}
                        helperText={errorText ?? undefined}
                      />
                    )}
                  />
                  {submitErrorCode !== null && (
                    <Alert
                      severity="error"
                      role="alert"
                      sx={{ width: '100%' }}
                    >
                      {submitErrorText(submitErrorCode)}
                    </Alert>
                  )}
                </FormLayout>
              )}
              <DataTable
                rows={teamQuery.data?.invitations ?? []}
                columns={invitationColumns}
                rowKey={(invitation) => invitation.id}
                loading={teamQuery.isFetching}
                emptyTitle={t('team.invitations.emptyTitle')}
                emptyDescription={t('team.invitations.emptyDescription')}
              />
            </section>
          </Box>
        </RouteGuard>
      )}
    </Box>
  )
}
