import { useState } from "react";
import type { FormEvent } from "react";
import {
  HardDriveIcon,
  MoreIcon,
  PlusIcon,
  RefreshIcon,
  ServerIcon,
  TrashIcon,
} from "../components/icons";
import { queryKeys, useImages, usePullImage, useRemoveImage } from "../lib/queries";
import type { BaseDisk } from "../lib/types";
import { Button, DropdownMenu, Field, IconButton, Input, Modal, Switch, cn, useToast } from "../ui";
import { describeError } from "../utils/errors";
import { formatBytes, formatDateTime, imageLabel, shortDigest } from "../utils/format";
import { useQueryClient } from "@tanstack/react-query";

interface RemoveTarget {
  image: BaseDisk;
  force: boolean;
}

function imageRef(image: BaseDisk): string {
  return image.tag !== undefined && image.tag !== "" ? image.tag : image.digest;
}

export interface ImagesPageProps {
  /** Open the pull dialog when arriving from the empty Create VM dialog. */
  autoOpenPull?: boolean;
  /** Called after autoOpenPull has been applied, so it only fires once. */
  onAutoOpenConsumed?: () => void;
  /** Open Create VM with this image preselected. */
  onCreateVm?: (image: BaseDisk) => void;
}

/** Image store: list cached base disks, pull new images, and remove them. */
export default function ImagesPage({
  autoOpenPull = false,
  onAutoOpenConsumed,
  onCreateVm,
}: ImagesPageProps) {
  const toast = useToast();
  const queryClient = useQueryClient();
  const images = useImages();
  const pullImage = usePullImage();
  const removeImage = useRemoveImage();

  const [pullOpen, setPullOpen] = useState(autoOpenPull);
  const [ref, setRef] = useState("");
  const [sizeGb, setSizeGb] = useState("");
  const [pullError, setPullError] = useState<string | null>(null);
  const [removeTarget, setRemoveTarget] = useState<RemoveTarget | null>(null);

  const closePull = () => {
    setPullOpen(false);
    onAutoOpenConsumed?.();
  };

  const list = images.data ?? [];
  const pulling = pullImage.isPending;
  const removing = removeImage.isPending;

  const handlePull = async () => {
    setPullError(null);
    const trimmed = ref.trim();
    if (trimmed === "") {
      setPullError("Image reference is required.");
      return;
    }
    let size: number | undefined;
    if (sizeGb.trim() !== "") {
      size = Number(sizeGb);
      if (!Number.isInteger(size) || size < 1 || size > 1024) {
        setPullError(
          "Size must be a whole number between 1 and 1024 GiB, or empty for the daemon default.",
        );
        return;
      }
    }
    try {
      const disk = await pullImage.mutateAsync({ ref: trimmed, size_gb: size });
      toast.success(`Pulled ${imageLabel(disk)}`);
      setRef("");
      setSizeGb("");
      closePull();
    } catch (error) {
      setPullError(describeError(error));
    }
  };

  const handleRemove = async () => {
    if (removeTarget === null) {
      return;
    }
    try {
      await removeImage.mutateAsync({
        ref: imageRef(removeTarget.image),
        force: removeTarget.force,
      });
      toast.success(`Removed ${imageLabel(removeTarget.image)}`);
      setRemoveTarget(null);
    } catch (error) {
      toast.error(describeError(error));
    }
  };

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.images });
  };

  return (
    <div className="space-y-6">
      <section className="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-gray-800 dark:bg-gray-900">
        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-gray-200 px-4 py-4 sm:px-6 dark:border-gray-800">
          <div>
            <h2 className="text-sm font-semibold text-gray-900 dark:text-white">Base images</h2>
            <p className="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
              {list.length} image{list.length === 1 ? "" : "s"} · read-only base disks shared by
              sandboxes
            </p>
          </div>
          <div className="flex items-center gap-2">
            <Button
              variant="primary"
              icon={<PlusIcon className="size-4" />}
              onClick={() => {
                setPullError(null);
                setPullOpen(true);
              }}
            >
              Pull image
            </Button>
            <IconButton label="Refresh images" onClick={refresh} disabled={images.isFetching}>
              <RefreshIcon className={images.isFetching ? "size-5 spinning" : "size-5"} />
            </IconButton>
          </div>
        </div>

        {images.isError ? (
          <div className="px-4 py-10 text-center sm:px-6">
            <p className="text-sm text-red-600 dark:text-red-400">
              Could not load images: {describeError(images.error)}
            </p>
            <Button variant="secondary" className="mt-3" onClick={refresh}>
              Retry
            </Button>
          </div>
        ) : images.isPending ? (
          <div className="px-4 py-10 text-center text-sm text-gray-500 sm:px-6 dark:text-gray-400">
            Loading images…
          </div>
        ) : list.length === 0 ? (
          <div className="px-4 py-10 text-center sm:px-6">
            <HardDriveIcon className="mx-auto size-8 text-gray-400 dark:text-gray-500" />
            <p className="mt-2 text-sm text-gray-500 dark:text-gray-400">
              No base images yet. Pull one above to get started.
            </p>
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-gray-200 bg-gray-50 text-xs uppercase tracking-wide text-gray-500 dark:border-gray-800 dark:bg-gray-800/60 dark:text-gray-400">
                <tr>
                  <th className="px-4 py-3 font-medium sm:px-6">Image</th>
                  <th className="px-4 py-3 font-medium">Digest</th>
                  <th className="px-4 py-3 font-medium">Size</th>
                  <th className="px-4 py-3 font-medium">Source</th>
                  <th className="px-4 py-3 font-medium">Created</th>
                  <th className="px-4 py-3 text-right font-medium sm:px-6">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-200 dark:divide-gray-800">
                {list.map((image) => (
                  <tr key={image.digest} className="hover:bg-gray-50 dark:hover:bg-gray-800/60">
                    <td className="px-4 py-3 sm:px-6">
                      <div className="flex flex-col gap-0.5">
                        <span className="font-medium text-gray-900 dark:text-white">
                          {imageLabel(image)}
                        </span>
                        {image.tags !== undefined && image.tags.length > 1 ? (
                          <span className="text-xs text-gray-500 dark:text-gray-400">
                            {image.tags.length} tags
                          </span>
                        ) : null}
                      </div>
                    </td>
                    <td className="px-4 py-3 font-mono text-xs text-gray-600 dark:text-gray-300">
                      {shortDigest(image.digest, 20)}
                    </td>
                    <td className="px-4 py-3 whitespace-nowrap text-gray-600 dark:text-gray-300">
                      <span title={`${formatBytes(image.size_bytes)} virtual disk`}>
                        {formatBytes(image.allocated_bytes ?? image.size_bytes)} used
                      </span>
                    </td>
                    <td className="px-4 py-3 text-gray-600 dark:text-gray-300">{image.source}</td>
                    <td className="px-4 py-3 whitespace-nowrap text-gray-600 dark:text-gray-300">
                      {formatDateTime(image.created_at)}
                    </td>
                    <td className="px-4 py-3 text-right sm:px-6">
                      <div className="flex justify-end gap-1">
                        {onCreateVm !== undefined ? (
                          <Button
                            variant="secondary"
                            size="sm"
                            aria-label={`Create VM from ${imageLabel(image)}`}
                            icon={<ServerIcon className="size-4" />}
                            onClick={() => {
                              onCreateVm(image);
                            }}
                          >
                            Create VM
                          </Button>
                        ) : null}
                        <DropdownMenu
                          label={`Actions for ${imageLabel(image)}`}
                          trigger={<MoreIcon className="size-5" />}
                          triggerClassName="inline-flex size-9 items-center justify-center rounded-lg text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-900 focus-visible:ring-4 focus-visible:ring-indigo-200 focus-visible:outline-none dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-white dark:focus-visible:ring-indigo-900"
                          items={[
                            {
                              key: "remove",
                              label: "Remove image",
                              icon: <TrashIcon className="size-4" />,
                              danger: true,
                              onClick: () => {
                                setRemoveTarget({ image, force: false });
                              },
                            },
                          ]}
                        />
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <Modal
        open={pullOpen}
        onClose={() => {
          if (!pulling) {
            closePull();
          }
        }}
        title="Pull image"
        description="Fetch an OCI image and build a bootable base disk. The first pull can take a while."
        footer={
          <>
            <Button onClick={closePull} disabled={pulling}>
              Cancel
            </Button>
            <Button
              variant="primary"
              type="submit"
              form="pull-image-form"
              loading={pulling}
              icon={<PlusIcon className="size-4" />}
            >
              Pull image
            </Button>
          </>
        }
      >
        <form
          id="pull-image-form"
          className="flex flex-col gap-4"
          onSubmit={(event: FormEvent<HTMLFormElement>) => {
            event.preventDefault();
            void handlePull();
          }}
        >
          <Field label="Image reference" htmlFor="pull-image-ref" error={pullError ?? undefined}>
            <Input
              id="pull-image-ref"
              placeholder="alpine:3.20 or ghcr.io/org/image:tag"
              autoComplete="off"
              value={ref}
              disabled={pulling}
              onChange={(event) => {
                setRef(event.target.value);
                setPullError(null);
              }}
            />
          </Field>
          <Field
            label="Size (GiB, optional)"
            htmlFor="pull-image-size"
            hint="Empty uses the daemon storage.root_size_gb default."
          >
            <Input
              id="pull-image-size"
              type="number"
              min={1}
              max={1024}
              step={1}
              value={sizeGb}
              disabled={pulling}
              onChange={(event) => {
                setSizeGb(event.target.value);
              }}
            />
          </Field>
        </form>
      </Modal>

      <Modal
        open={removeTarget !== null}
        onClose={() => {
          if (!removing) {
            setRemoveTarget(null);
          }
        }}
        size="sm"
        title="Remove this image?"
        footer={
          <>
            <Button onClick={() => setRemoveTarget(null)} disabled={removing}>
              Cancel
            </Button>
            <Button variant="danger" loading={removing} onClick={() => void handleRemove()}>
              Remove image
            </Button>
          </>
        }
      >
        <p className="text-sm leading-relaxed text-gray-600 dark:text-gray-300">
          <strong className="font-semibold text-gray-900 dark:text-white">
            {removeTarget === null ? "This image" : imageLabel(removeTarget.image)}
          </strong>{" "}
          and every tag pointing at it will be deleted. Sandboxes that still reference it must be
          removed first unless force is enabled.
        </p>
        <div className="mt-4 flex items-center gap-3">
          <Switch
            id="remove-image-force"
            checked={removeTarget?.force ?? false}
            disabled={removing}
            onChange={(force) => {
              setRemoveTarget((current) => (current === null ? current : { ...current, force }));
            }}
          />
          <label htmlFor="remove-image-force" className="text-sm text-gray-700 dark:text-gray-300">
            Force remove even if sandboxes reference it
          </label>
        </div>
      </Modal>

      <div className={cn("text-xs text-gray-500 dark:text-gray-400", removing ? "opacity-60" : "")}>
        Images are content-addressed and shared by all sandboxes.
      </div>
    </div>
  );
}
