/**
 * cases-errors.ts -- the cases surface's reachable-error whitelist: one
 * map from every code the cases surface can be answered with to the
 * app-namespace key carrying its current-language text, plus the small
 * classifier the surfaces share.
 *
 * The code set is the server's own: the cases fragment's documented
 * answers (internal/cases's service errors -- patient-name, photo-
 * object and duplicate/not-found codes -- plus the surface-level photo
 * upload/content codes internal/app/cases_photos.go defines and the
 * invalid-request-body/internal envelopes every handler can write) and
 * the transport's client.* codes. An answer that is not on the list (a
 * future server code, a client.http.<status> transport answer) resolves
 * to the unknown fallback, so the surface never shows a raw key or
 * another language's text -- the identical discipline the notes
 * surface's whitelist follows.
 *
 * Which codes a given view can actually receive is narrower than the
 * map (the list read can never receive a patient-name refusal, the
 * create form can never receive photo_not_found), but the map is keyed
 * by code, not by view: one code has one text wherever it surfaces.
 * Exported so the surface's whitelist can be deep-imported by the
 * codes-alignment suite like the notes surface's is.
 */
export const CASES_ERROR_TEXT_KEYS: Readonly<Record<string, string>> = {
  // The create route's refusals (internal/cases/service.go).
  'cases.patient_name_required': 'cases.errors.patientNameRequired',
  'cases.patient_name_too_long': 'cases.errors.patientNameTooLong',
  'cases.photo_object_id_required': 'cases.errors.photoObjectIdRequired',
  'cases.photo_object_id_too_long': 'cases.errors.photoObjectIdTooLong',
  'cases.duplicate_photo_object': 'cases.errors.duplicatePhotoObject',
  'cases.too_many_photos': 'cases.errors.tooManyPhotos',
  'cases.photo_already_attached': 'cases.errors.photoAlreadyAttached',
  // The photo-upload route's refusals (internal/app/cases_photos.go).
  'cases.photo_content_required': 'cases.errors.photoContentRequired',
  'cases.photo_content_invalid': 'cases.errors.photoContentInvalid',
  'cases.photo_content_too_large': 'cases.errors.photoContentTooLarge',
  'cases.photo_rejected': 'cases.errors.photoRejected',
  // The reads' refusals: a case or photo the caller's tenant cannot see.
  'cases.not_found': 'cases.errors.notFound',
  'cases.photo_not_found': 'cases.errors.photoNotFound',
  // The request could not be attributed to a user (create only).
  'cases.subject_unresolved': 'cases.errors.subjectUnresolved',
  // The handler-level envelopes every operation can write.
  'cases.invalid_request_body': 'cases.errors.invalidRequest',
  'cases.internal_error': 'cases.errors.internalError',
  // The transport's own codes (the client.* trio the request function
  // can emit); a client.http.<status> answer is not whitelisted and
  // degrades to the unknown fallback, exactly as on the notes surface.
  'client.network': 'cases.errors.client',
  'client.timeout': 'cases.errors.client',
  'client.protocol': 'cases.errors.client',
}

/** The text key of a code the surface was answered with, falling back
 * to the unknown key for codes outside the whitelist. */
export function casesErrorTextKey(code: string): string {
  return CASES_ERROR_TEXT_KEYS[code] ?? 'cases.errors.unknown'
}

/**
 * The code of an ApiError-shaped failure, or null for a failure that
 * carries none (a bug-shaped throw, an un-normalized answer).
 */
export function apiErrorCodeOf(error: unknown): string | null {
  if (typeof error !== 'object' || error === null) {
    return null
  }
  const code = (error as { code?: unknown }).code
  return typeof code === 'string' && code.length > 0 ? code : null
}

/**
 * The submit path's failure classifier: an ApiError-shaped failure
 * keeps its code, anything else -- a bug-shaped throw, an un-normalized
 * answer -- collapses to a code that is deliberately not whitelisted,
 * so the resolver renders the unknown fallback. A submit that throws at
 * all always has a code to show.
 */
export function casesErrorCodeOf(error: unknown): string {
  return apiErrorCodeOf(error) ?? 'client.unknown'
}
