SELECT jsonb_build_object(
    'user_photos', EXISTS (SELECT 1 FROM public.user_photos),
    'profile_photo_state', EXISTS (SELECT 1 FROM public.profile_photo_state),
    'profile_upload_receipt', EXISTS (SELECT 1 FROM public.profile_upload_receipt),
    'profile_delete_operation', EXISTS (SELECT 1 FROM public.profile_delete_operation),
    'erasure_outbox', EXISTS (SELECT 1 FROM public.erasure_outbox),
    'erasure_epoch', EXISTS (SELECT 1 FROM public.erasure_epoch),
    'erasure_epoch_completion', EXISTS (SELECT 1 FROM public.erasure_epoch_completion)
)::text;
