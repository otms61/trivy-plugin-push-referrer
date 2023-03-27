package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/aquasecurity/trivy/pkg/log"
	"github.com/aquasecurity/trivy/pkg/purl"
	"github.com/aquasecurity/trivy/pkg/sbom"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	ctypes "github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/spdx/tools-golang/spdx"
	"github.com/spf13/cobra"
)

const (
	// ref. https://github.com/opencontainers/image-spec/blob/dd7fd714f5406d39db5fd0602a0e6090929dc85e/annotations.md#pre-defined-annotation-keys
	annotationKeyDescription = "org.opencontainers.artifact.description"

	// ref. https://www.iana.org/assignments/media-types/media-types.xhtml
	// ref. https://www.iana.org/assignments/media-types/media-types.xhtml
	mediaKeyCycloneDX = "application/vnd.cyclonedx+json"
	mediaKeySPDX      = "application/spdx+json"
)

type referrer struct {
	annotations map[string]string
	mediaType   ctypes.MediaType
	bytes       []byte
	targetRepo  name.Digest
	targetDesc  v1.Descriptor
}

func (r *referrer) Image() (v1.Image, error) {
	img, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer: static.NewLayer(r.bytes, ctypes.OCIUncompressedLayer),
	})
	if err != nil {
		return nil, err
	}

	// https://github.com/opencontainers/image-spec/blob/dd7fd714f5406d39db5fd0602a0e6090929dc85e/artifact.md#artifact-manifest-property-descriptions
	img = mutate.MediaType(img, r.targetDesc.MediaType)
	img = mutate.ConfigMediaType(img, r.mediaType)
	img = mutate.Annotations(img, r.annotations).(v1.Image)
	img = mutate.Subject(img, r.targetDesc).(v1.Image)

	return img, nil
}

func (r *referrer) Tag(img v1.Image) (name.Digest, error) {
	digest, err := img.Digest()
	if err != nil {
		return name.Digest{}, err
	}

	tag, err := name.NewDigest(
		fmt.Sprintf("%s/%s@%s", r.targetRepo.RegistryStr(), r.targetRepo.RepositoryStr(), digest.String()),
	)
	if err != nil {
		return name.Digest{}, err
	}
	return tag, nil
}

func repoFromPurl(purlStr string) (name.Digest, error) {
	p, err := purl.FromString(purlStr)
	if err != nil {
		return name.Digest{}, err
	}

	url := p.Qualifiers.Map()["repository_url"]
	if url == "" {
		return name.Digest{}, fmt.Errorf("repository_url not found")
	}

	digest, err := name.NewDigest(fmt.Sprintf("%s@%s", url, p.Version))
	if err != nil {
		return name.Digest{}, err
	}

	return digest, nil
}

func repoFromSpdx(spdx spdx.Document2_2) (name.Digest, error) {
	for _, pkg := range spdx.Packages {
		if pkg.PackageName == spdx.CreationInfo.DocumentName {
			for _, ref := range pkg.PackageExternalReferences {
				if ref.Category == "PACKAGE-MANAGER" {
					return repoFromPurl(ref.Locator)
				}
			}
		}
	}

	return name.Digest{}, fmt.Errorf("not found: repo uri")
}

func referrerFromSBOM(r io.Reader) (referrer, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return referrer{}, err
	}

	format, err := sbom.DetectFormat(bytes.NewReader(b))
	if err != nil {
		return referrer{}, err
	}
	decoded, err := sbom.Decode(bytes.NewReader(b), format)
	if err != nil {
		return referrer{}, err
	}

	var mediaType ctypes.MediaType
	var anns map[string]string
	var repo name.Digest

	switch format {
	case sbom.FormatCycloneDXJSON:
		repo, err = repoFromPurl(decoded.CycloneDX.Metadata.Component.BOMRef)
		if err != nil {
			return referrer{}, err
		}
		anns = map[string]string{
			annotationKeyDescription: "CycloneDX JSON SBOM",
		}
		mediaType = mediaKeyCycloneDX

	case sbom.FormatSPDXJSON:
		repo, err = repoFromSpdx(*decoded.SPDX)
		if err != nil {
			return referrer{}, err
		}
		anns = map[string]string{
			annotationKeyDescription: "SPDX JSON SBOM",
		}
		mediaType = mediaKeySPDX

	default:
		return referrer{}, fmt.Errorf("unsupported format: %s", format)
	}

	targetDesc, err := remote.Head(repo)
	if err != nil {
		return referrer{}, err
	}

	return referrer{
		annotations: anns,
		mediaType:   mediaType,
		bytes:       b,
		targetRepo:  repo,
		targetDesc:  *targetDesc,
	}, nil
}

func putReferrer(r io.Reader) error {
	ref, err := referrerFromSBOM(r)
	if err != nil {
		return err
	}

	img, err := ref.Image()
	if err != nil {
		return err
	}

	tag, err := ref.Tag(img)
	if err != nil {
		return err
	}

	log.Logger.Debugf("Pushing referrer to %s", tag.String())

	err = remote.Write(tag, img, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return err
	}

	return nil
}

func main() {
	rootCmd := &cobra.Command{
		Short: "A Trivy plugin for oci referrers",
	}
	putCmd := &cobra.Command{
		Use:   "put",
		Short: "put a referrer to the oci registry",
		Example: `  trivy image -q -f cyclonedx YOUR_IMAGE | trivy referrer put
  # Put SBOM attestation
  trivy referrer put -f sbom.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := cmd.Flags().GetString("file")
			if err != nil {
				return err
			}

			var reader io.Reader
			if path != "" {
				fp, err := os.Open(path)
				if err != nil {
					return err
				}
				defer fp.Close()

				reader = fp
			} else {
				reader = os.Stdin
			}

			err = putReferrer(reader)
			if err != nil {
				return err
			}

			return nil
		},
	}
	putCmd.Flags().StringP("file", "f", "", "SBOM file path")

	rootCmd.AddCommand(putCmd)

	if err := putCmd.Execute(); err != nil {
		log.Logger.Fatal(err)
	}
}
