package aigateway

import "testing"

func TestImageRequest_Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     ImageRequest
		wantErr string // apperr code, empty means no error
	}{
		{
			name:    "empty model",
			req:     ImageRequest{Prompt: "a smile", Operation: ImageOperationTextToImage},
			wantErr: ErrEmptyModel.Code,
		},
		{
			name:    "empty prompt",
			req:     ImageRequest{Model: "image:default", Operation: ImageOperationTextToImage},
			wantErr: ErrEmptyPrompt.Code,
		},
		{
			name:    "unknown operation",
			req:     ImageRequest{Model: "image:default", Prompt: "a smile", Operation: "not-a-real-operation"},
			wantErr: ErrInvalidImageOperation.Code,
		},
		{
			name: "text-to-image valid",
			req: ImageRequest{
				Model: "image:default", Prompt: "a smile",
				Operation: ImageOperationTextToImage,
			},
		},
		{
			name: "text-to-image with input object id refused",
			req: ImageRequest{
				Model: "image:default", Prompt: "a smile",
				Operation: ImageOperationTextToImage, InputObjectID: "obj-1",
			},
			wantErr: ErrImageInputNotAllowed.Code,
		},
		{
			name: "text-to-image with mask object id refused",
			req: ImageRequest{
				Model: "image:default", Prompt: "a smile",
				Operation: ImageOperationTextToImage, MaskObjectID: "mask-1",
			},
			wantErr: ErrImageInputNotAllowed.Code,
		},
		{
			name: "image-to-image valid",
			req: ImageRequest{
				Model: "image:default", Prompt: "a smile",
				Operation: ImageOperationImageToImage, InputObjectID: "obj-1",
			},
		},
		{
			name: "image-to-image missing input refused",
			req: ImageRequest{
				Model: "image:default", Prompt: "a smile",
				Operation: ImageOperationImageToImage,
			},
			wantErr: ErrImageInputRequired.Code,
		},
		{
			name: "image-to-image with mask refused",
			req: ImageRequest{
				Model: "image:default", Prompt: "a smile",
				Operation: ImageOperationImageToImage, InputObjectID: "obj-1", MaskObjectID: "mask-1",
			},
			wantErr: ErrImageInputNotAllowed.Code,
		},
		{
			name: "inpaint valid",
			req: ImageRequest{
				Model: "image:default", Prompt: "fix the teeth",
				Operation: ImageOperationInpaint, InputObjectID: "obj-1", MaskObjectID: "mask-1",
			},
		},
		{
			name: "inpaint missing input refused",
			req: ImageRequest{
				Model: "image:default", Prompt: "fix the teeth",
				Operation: ImageOperationInpaint, MaskObjectID: "mask-1",
			},
			wantErr: ErrImageInputRequired.Code,
		},
		{
			name: "inpaint missing mask refused",
			req: ImageRequest{
				Model: "image:default", Prompt: "fix the teeth",
				Operation: ImageOperationInpaint, InputObjectID: "obj-1",
			},
			wantErr: ErrImageMaskRequired.Code,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			got, ok := apperrCode(err)
			if !ok || got != tt.wantErr {
				t.Fatalf("validate() = %v, want code %q", err, tt.wantErr)
			}
		})
	}
}
