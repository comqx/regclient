package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/internal/diff"
	"github.com/regclient/regclient/pkg/template"
	"github.com/regclient/regclient/types"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/platform"
	"github.com/regclient/regclient/types/ref"
)

type manifestCmd struct {
	rootOpts      *rootCmd
	byDigest      bool
	contentType   string
	diffCtx       int
	diffFullCtx   bool
	forceTagDeref bool
	formatGet     string
	formatHead    string
	formatPut     string
	list          bool
	platform      string
	referrers     bool
	requireDigest bool
	requireList   bool
}

func NewManifestCmd(rootOpts *rootCmd) *cobra.Command {
	manifestOpts := manifestCmd{
		rootOpts: rootOpts,
	}
	var manifestTopCmd = &cobra.Command{
		Use:   "manifest <cmd>",
		Short: "manage manifests",
	}

	var manifestDeleteCmd = &cobra.Command{
		Use:     "delete <image_ref>",
		Aliases: []string{"del", "rm", "remove"},
		Short:   "delete a manifest",
		Long: `Delete a manifest. This will delete the manifest, and all tags pointing to that
manifest. You must specify a digest, not a tag on this command (e.g. 
image_name@sha256:1234abc...). It is up to the registry whether the delete
API is supported. Additionally, registries may garbage collect the filesystem
layers (blobs) separately or not at all. See also the "tag delete" command.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{}, // do not auto complete digests
		RunE:      manifestOpts.runManifestDelete,
	}

	var manifestDiffCmd = &cobra.Command{
		Use:               "diff <image_ref> <image_ref>",
		Short:             "compare manifests",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: rootOpts.completeArgTag,
		RunE:              manifestOpts.runManifestDiff,
	}

	var manifestGetCmd = &cobra.Command{
		Use:               "get <image_ref>",
		Aliases:           []string{"pull"},
		Short:             "retrieve manifest or manifest list",
		Long:              `Shows the manifest or manifest list of the specified image.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: rootOpts.completeArgTag,
		RunE:              manifestOpts.runManifestGet,
	}

	var manifestHeadCmd = &cobra.Command{
		Use:               "head <image_ref>",
		Aliases:           []string{"digest"},
		Short:             "http head request for manifest",
		Long:              `Shows the digest or headers from an http manifest head request.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: rootOpts.completeArgTag,
		RunE:              manifestOpts.runManifestHead,
	}

	var manifestPutCmd = &cobra.Command{
		Use:               "put <image_ref>",
		Aliases:           []string{"push"},
		Short:             "push manifest or manifest list",
		Long:              `Pushes a manifest or manifest list to a repository.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: rootOpts.completeArgTag,
		RunE:              manifestOpts.runManifestPut,
	}

	manifestDeleteCmd.Flags().BoolVarP(&manifestOpts.forceTagDeref, "force-tag-dereference", "", false, "Dereference the a tag to a digest, this is unsafe")
	manifestDeleteCmd.Flags().BoolVarP(&manifestOpts.referrers, "referrers", "", false, "Check for referrers, recommended when deleting artifacts")

	manifestDiffCmd.Flags().IntVarP(&manifestOpts.diffCtx, "context", "", 3, "Lines of context")
	manifestDiffCmd.Flags().BoolVarP(&manifestOpts.diffFullCtx, "context-full", "", false, "Show all lines of context")

	manifestHeadCmd.Flags().StringVarP(&manifestOpts.formatHead, "format", "", "", "Format output with go template syntax (use \"raw-body\" for the original manifest)")
	manifestHeadCmd.Flags().BoolVarP(&manifestOpts.list, "list", "", true, "Do not resolve platform from manifest list (enabled by default)")
	manifestHeadCmd.Flags().StringVarP(&manifestOpts.platform, "platform", "p", "", "Specify platform (e.g. linux/amd64 or local)")
	manifestHeadCmd.Flags().BoolVarP(&manifestOpts.requireDigest, "require-digest", "", false, "Fallback to get request if digest is not received")
	manifestHeadCmd.Flags().BoolVarP(&manifestOpts.requireList, "require-list", "", false, "Fail if manifest list is not received")
	_ = manifestHeadCmd.RegisterFlagCompletionFunc("platform", completeArgPlatform)
	_ = manifestHeadCmd.Flags().MarkHidden("list")

	manifestGetCmd.Flags().BoolVarP(&manifestOpts.list, "list", "", true, "Output manifest list if available (enabled by default)")
	manifestGetCmd.Flags().StringVarP(&manifestOpts.platform, "platform", "p", "", "Specify platform (e.g. linux/amd64 or local)")
	manifestGetCmd.Flags().BoolVarP(&manifestOpts.requireList, "require-list", "", false, "Fail if manifest list is not received")
	manifestGetCmd.Flags().StringVarP(&manifestOpts.formatGet, "format", "", "{{printPretty .}}", "Format output with go template syntax (use \"raw-body\" for the original manifest)")
	_ = manifestGetCmd.RegisterFlagCompletionFunc("platform", completeArgPlatform)
	_ = manifestGetCmd.RegisterFlagCompletionFunc("format", completeArgNone)
	_ = manifestGetCmd.Flags().MarkHidden("list")

	manifestPutCmd.Flags().BoolVarP(&manifestOpts.byDigest, "by-digest", "", false, "Push manifest by digest instead of tag")
	manifestPutCmd.Flags().StringVarP(&manifestOpts.contentType, "content-type", "t", "", "Specify content-type (e.g. application/vnd.docker.distribution.manifest.v2+json)")
	_ = manifestPutCmd.RegisterFlagCompletionFunc("content-type", completeArgMediaTypeManifest)
	manifestPutCmd.Flags().StringVarP(&manifestOpts.formatPut, "format", "", "", "Format output with go template syntax")

	manifestTopCmd.AddCommand(manifestDeleteCmd)
	manifestTopCmd.AddCommand(manifestDiffCmd)
	manifestTopCmd.AddCommand(manifestHeadCmd)
	manifestTopCmd.AddCommand(manifestGetCmd)
	manifestTopCmd.AddCommand(manifestPutCmd)
	return manifestTopCmd
}

func (manifestOpts *manifestCmd) runManifestDelete(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	r, err := ref.New(args[0])
	if err != nil {
		return err
	}
	rc := manifestOpts.rootOpts.newRegClient()
	defer rc.Close(ctx, r)

	if r.Digest == "" && manifestOpts.forceTagDeref {
		m, err := rc.ManifestHead(ctx, r, regclient.WithManifestRequireDigest())
		if err != nil {
			return err
		}
		r.Digest = manifest.GetDigest(m).String()
		log.WithFields(logrus.Fields{
			"tag":    r.Tag,
			"digest": r.Digest,
		}).Debug("Forced dereference of tag")
	}

	log.WithFields(logrus.Fields{
		"host":   r.Registry,
		"repo":   r.Repository,
		"digest": r.Digest,
	}).Debug("Manifest delete")
	mOpts := []regclient.ManifestOpts{}
	if manifestOpts.referrers {
		mOpts = append(mOpts, regclient.WithManifestCheckReferrers())
	}

	err = rc.ManifestDelete(ctx, r, mOpts...)
	if err != nil {
		return err
	}
	return nil
}

func (manifestOpts *manifestCmd) runManifestDiff(cmd *cobra.Command, args []string) error {
	diffOpts := []diff.Opt{}
	if manifestOpts.diffCtx > 0 {
		diffOpts = append(diffOpts, diff.WithContext(manifestOpts.diffCtx, manifestOpts.diffCtx))
	}
	if manifestOpts.diffFullCtx {
		diffOpts = append(diffOpts, diff.WithFullContext())
	}
	ctx := cmd.Context()
	r1, err := ref.New(args[0])
	if err != nil {
		return err
	}
	r2, err := ref.New(args[1])
	if err != nil {
		return err
	}

	rc := manifestOpts.rootOpts.newRegClient()

	log.WithFields(logrus.Fields{
		"ref1": r1.CommonName(),
		"ref2": r2.CommonName(),
	}).Debug("Manifest diff")

	m1, err := rc.ManifestGet(ctx, r1)
	if err != nil {
		return err
	}
	m2, err := rc.ManifestGet(ctx, r2)
	if err != nil {
		return err
	}

	m1Json, err := json.MarshalIndent(m1, "", "  ")
	if err != nil {
		return err
	}
	m2Json, err := json.MarshalIndent(m2, "", "  ")
	if err != nil {
		return err
	}

	mDiff := diff.Diff(strings.Split(string(m1Json), "\n"), strings.Split(string(m2Json), "\n"), diffOpts...)

	_, err = fmt.Fprintln(cmd.OutOrStdout(), strings.Join(mDiff, "\n"))
	return err
	// TODO: support templating
	// return template.Writer(cmd.OutOrStdout(), manifestOpts.format, mDiff)
}

func (manifestOpts *manifestCmd) runManifestHead(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if manifestOpts.platform != "" && !flagChanged(cmd, "list") {
		manifestOpts.list = false
	} else if !manifestOpts.list && !flagChanged(cmd, "list") {
		manifestOpts.list = true
	}

	r, err := ref.New(args[0])
	if err != nil {
		return err
	}
	rc := manifestOpts.rootOpts.newRegClient()

	log.WithFields(logrus.Fields{
		"host": r.Registry,
		"repo": r.Repository,
		"tag":  r.Tag,
	}).Debug("Manifest head")

	mOpts := []regclient.ManifestOpts{}
	if manifestOpts.requireDigest || (!flagChanged(cmd, "require-digest") && !flagChanged(cmd, "format")) {
		mOpts = append(mOpts, regclient.WithManifestRequireDigest())
	}

	// attempt to request only the headers, avoids Docker Hub rate limits
	m, err := rc.ManifestHead(ctx, r, mOpts...)
	if err != nil {
		return err
	}

	// add warning if not list and list required or platform requested
	if !m.IsList() && manifestOpts.requireList {
		log.Warn("Manifest list unavailable")
		return ErrNotFound
	}
	if !m.IsList() && manifestOpts.platform != "" {
		log.Info("Manifest list unavailable, ignoring platform flag")
	}

	// retrieve the specified platform from the manifest list
	for m.IsList() && !manifestOpts.list && !manifestOpts.requireList {
		desc, err := getPlatformDesc(ctx, rc, m, manifestOpts.platform)
		if err != nil {
			return fmt.Errorf("failed retrieving platform specific digest: %w", err)
		}
		r.Digest = desc.Digest.String()
		m, err = rc.ManifestHead(ctx, r, mOpts...)
		if err != nil {
			return fmt.Errorf("failed retrieving platform specific digest: %w", err)
		}
	}

	switch manifestOpts.formatHead {
	case "", "digest":
		manifestOpts.formatHead = "{{ printf \"%s\\n\" .GetDescriptor.Digest }}"
	case "rawHeaders", "raw-headers", "headers":
		manifestOpts.formatHead = "{{ range $key,$vals := .RawHeaders}}{{range $val := $vals}}{{printf \"%s: %s\\n\" $key $val }}{{end}}{{end}}"
	}
	return template.Writer(cmd.OutOrStdout(), manifestOpts.formatHead, m)
}

func (manifestOpts *manifestCmd) runManifestGet(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if manifestOpts.platform != "" && !flagChanged(cmd, "list") {
		manifestOpts.list = false
	} else if !manifestOpts.list && !flagChanged(cmd, "list") {
		manifestOpts.list = true
	}

	r, err := ref.New(args[0])
	if err != nil {
		return err
	}
	rc := manifestOpts.rootOpts.newRegClient()
	defer rc.Close(ctx, r)

	m, err := getManifest(ctx, rc, r, manifestOpts.platform, manifestOpts.list, manifestOpts.requireList)
	if err != nil {
		return err
	}

	switch manifestOpts.formatGet {
	case "raw":
		manifestOpts.formatGet = "{{ range $key,$vals := .RawHeaders}}{{range $val := $vals}}{{printf \"%s: %s\\n\" $key $val }}{{end}}{{end}}{{printf \"\\n%s\" .RawBody}}"
	case "rawBody", "raw-body", "body":
		manifestOpts.formatGet = "{{printf \"%s\" .RawBody}}"
	case "rawHeaders", "raw-headers", "headers":
		manifestOpts.formatGet = "{{ range $key,$vals := .RawHeaders}}{{range $val := $vals}}{{printf \"%s: %s\\n\" $key $val }}{{end}}{{end}}"
	}
	return template.Writer(cmd.OutOrStdout(), manifestOpts.formatGet, m)
}

func (manifestOpts *manifestCmd) runManifestPut(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	r, err := ref.New(args[0])
	if err != nil {
		return err
	}
	rc := manifestOpts.rootOpts.newRegClient()
	defer rc.Close(ctx, r)

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	opts := []manifest.Opts{
		manifest.WithRef(r),
		manifest.WithRaw(raw),
	}
	if manifestOpts.contentType != "" {
		opts = append(opts, manifest.WithDesc(types.Descriptor{
			MediaType: manifestOpts.contentType,
		}))
	}
	rcM, err := manifest.New(opts...)
	if err != nil {
		return err
	}
	if manifestOpts.byDigest {
		r.Tag = ""
		r.Digest = rcM.GetDescriptor().Digest.String()
	}

	err = rc.ManifestPut(ctx, r, rcM)
	if err != nil {
		return err
	}

	result := struct {
		Manifest manifest.Manifest
	}{
		Manifest: rcM,
	}
	if manifestOpts.byDigest && manifestOpts.formatPut == "" {
		manifestOpts.formatPut = "{{ printf \"%s\\n\" .Manifest.GetDescriptor.Digest }}"
	}
	return template.Writer(cmd.OutOrStdout(), manifestOpts.formatPut, result)
}

func getManifest(ctx context.Context, rc *regclient.RegClient, r ref.Ref, pStr string, list, reqList bool) (manifest.Manifest, error) {
	m, err := rc.ManifestGet(context.Background(), r)
	if err != nil {
		return m, err
	}

	// add warning if not list and list required or platform requested
	if !m.IsList() && reqList {
		log.Warn("Manifest list unavailable")
		return m, ErrNotFound
	}
	if !m.IsList() && pStr != "" {
		log.Info("Manifest list unavailable, ignoring platform flag")
	}

	// retrieve the specified platform from the manifest list
	if m.IsList() && !list && !reqList {
		desc, err := getPlatformDesc(ctx, rc, m, pStr)
		if err != nil {
			return m, fmt.Errorf("failed to lookup platform specific digest: %w", err)
		}
		m, err = rc.ManifestGet(ctx, r, regclient.WithManifestDesc(*desc))
		if err != nil {
			return m, fmt.Errorf("failed to pull platform specific digest: %w", err)
		}
	}
	return m, nil
}

func getPlatformDesc(ctx context.Context, rc *regclient.RegClient, m manifest.Manifest, pStr string) (*types.Descriptor, error) {
	var desc *types.Descriptor
	var err error
	if !m.IsList() {
		return desc, fmt.Errorf("%w: manifest is not a list", ErrInvalidInput)
	}
	if !m.IsSet() {
		m, err = rc.ManifestGet(ctx, m.GetRef())
		if err != nil {
			return desc, fmt.Errorf("unable to retrieve manifest list: %w", err)
		}
	}

	var plat platform.Platform
	if pStr != "" && pStr != "local" {
		plat, err = platform.Parse(pStr)
		if err != nil {
			log.WithFields(logrus.Fields{
				"platform": pStr,
				"err":      err,
			}).Warn("Could not parse platform")
		}
	}
	if plat.OS == "" {
		plat = platform.Local()
	}
	desc, err = manifest.GetPlatformDesc(m, &plat)
	if err != nil {
		pl, _ := manifest.GetPlatformList(m)
		var ps []string
		for _, p := range pl {
			ps = append(ps, p.String())
		}
		log.WithFields(logrus.Fields{
			"platform":  plat,
			"err":       err,
			"platforms": strings.Join(ps, ", "),
		}).Warn("Platform could not be found in manifest list")
		return desc, ErrNotFound
	}
	log.WithFields(logrus.Fields{
		"platform": plat,
		"digest":   desc.Digest.String(),
	}).Debug("Found platform specific digest in manifest list")
	return desc, nil
}
